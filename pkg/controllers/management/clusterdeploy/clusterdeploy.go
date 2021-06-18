package clusterdeploy

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/rancher/norman/types"
	v32 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	util "github.com/rancher/rancher/pkg/cluster"
	"github.com/rancher/rancher/pkg/clustermanager"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/image"
	"github.com/rancher/rancher/pkg/kubectl"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/systemaccount"
	"github.com/rancher/rancher/pkg/systemtemplate"
	"github.com/rancher/rancher/pkg/taints"
	"github.com/rancher/rancher/pkg/types/config"
	"github.com/rancher/rancher/pkg/user"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	AgentForceDeployAnn = "io.cattle.agent.force.deploy"
	nodeImage           = "nodeImage"
	clusterImage        = "clusterImage"
)

var (
	agentImagesMutex sync.RWMutex
	agentImages      = map[string]map[string]string{
		nodeImage:    map[string]string{},
		clusterImage: map[string]string{},
	}
	controlPlaneTaintsMutex sync.RWMutex
	controlPlaneTaints      = make(map[string][]corev1.Taint)
	controlPlaneLabels      = map[string]string{
		"node-role.kubernetes.io/master":        "true",
		"node-role.kubernetes.io/controlplane":  "true",
		"node-role.kubernetes.io/control-plane": "true",
	}
)

func Register(ctx context.Context, management *config.ManagementContext, clusterManager *clustermanager.Manager) {
	c := &clusterDeploy{
		systemAccountManager: systemaccount.NewManager(management),
		userManager:          management.UserManager,
		clusters:             management.Management.Clusters(""),
		nodeLister:           management.Management.Nodes("").Controller().Lister(),
		clusterManager:       clusterManager,
	}

	management.Management.Clusters("").AddHandler(ctx, "cluster-deploy", c.sync)
}

type clusterDeploy struct {
	systemAccountManager *systemaccount.Manager
	userManager          user.Manager
	clusters             v3.ClusterInterface
	clusterManager       *clustermanager.Manager
	nodeLister           v3.NodeLister
}

func (cd *clusterDeploy) sync(key string, cluster *v3.Cluster) (runtime.Object, error) {
	logrus.Tracef("clusterDeploy: sync called for key [%s]", key)
	var (
		err, updateErr error
	)

	if cluster == nil || cluster.DeletionTimestamp != nil {
		// remove the system account user created for this cluster
		if err := cd.systemAccountManager.RemoveSystemAccount(key); err != nil {
			return nil, err
		}
		return nil, nil
	}

	original := cluster
	cluster = original.DeepCopy()

	if cluster.Status.Driver == v32.ClusterDriverRKE {
		if cluster.Spec.LocalClusterAuthEndpoint.Enabled {
			cluster.Spec.RancherKubernetesEngineConfig.Authentication.Strategy = "x509|webhook"
		} else {
			cluster.Spec.RancherKubernetesEngineConfig.Authentication.Strategy = "x509"
		}
		logrus.Tracef("clusterDeploy: sync: cluster.Spec.RancherKubernetesEngineConfig.Authentication.Strategy set to [%s] for cluster [%s]", cluster.Spec.RancherKubernetesEngineConfig.Authentication.Strategy, cluster.Name)
	}

	err = cd.doSync(cluster)
	if cluster != nil && !reflect.DeepEqual(cluster, original) {
		logrus.Tracef("clusterDeploy: sync: cluster changed, calling Update on cluster [%s]", cluster.Name)
		_, updateErr = cd.clusters.Update(cluster)
	}

	if err != nil {
		return nil, err
	}
	return nil, updateErr
}

func (cd *clusterDeploy) doSync(cluster *v3.Cluster) error {
	logrus.Tracef("clusterDeploy: doSync called for cluster [%s]", cluster.Name)

	if !v32.ClusterConditionProvisioned.IsTrue(cluster) {
		logrus.Tracef("clusterDeploy: doSync: cluster [%s] is not yet provisioned (ClusterConditionProvisioned is not True)", cluster.Name)
		return nil
	}

	nodes, err := cd.nodeLister.List(cluster.Name, labels.Everything())
	if err != nil {
		return err
	}
	logrus.Tracef("clusterDeploy: doSync: found [%d] nodes for cluster [%s]", len(nodes), cluster.Name)

	if len(nodes) == 0 {
		return nil
	}

	_, err = v32.ClusterConditionSystemAccountCreated.DoUntilTrue(cluster, func() (runtime.Object, error) {
		logrus.Tracef("clusterDeploy: doSync: Creating SystemAccount for cluster [%s]", cluster.Name)
		return cluster, cd.systemAccountManager.CreateSystemAccount(cluster)
	})
	if err != nil {
		return err
	}

	if cluster.Status.AgentImage != "" && !agentImagesCached(cluster.Name) {
		if err := cd.cacheAgentImages(cluster.Name); err != nil {
			return err
		}
	}

	if !controlPlaneTaintsCached(cluster.Name) {
		if err := cd.cacheControlPlaneTaints(cluster.Name); err != nil {
			return err
		}
	}

	err = cd.deployAgent(cluster)
	if err != nil {
		return err
	}

	return cd.setNetworkPolicyAnn(cluster)
}

// agentFeaturesChanged will treat a missing key as false. This means we only detect changes
// when we set a feature to true so we can't reliably set a feature to false that is enabled by default.
// This behavior makes adding new def false features not cause the agent to redeploy.
func agentFeaturesChanged(desired, actual map[string]bool) bool {
	for k, v := range desired {
		if actual[k] != v {
			return true
		}
	}

	for k, v := range actual {
		if desired[k] != v {
			return true
		}
	}

	return false
}

func redeployAgent(cluster *v3.Cluster, desiredAgent, desiredAuth string, desiredFeatures map[string]bool, desiredTaints []corev1.Taint) bool {
	logrus.Tracef("clusterDeploy: redeployAgent called for cluster [%s]", cluster.Name)
	if !v32.ClusterConditionAgentDeployed.IsTrue(cluster) {
		return true
	}
	forceDeploy := cluster.Annotations[AgentForceDeployAnn] == "true"
	imageChange := cluster.Status.AgentImage != desiredAgent || cluster.Status.AuthImage != desiredAuth
	agentFeaturesChanged := agentFeaturesChanged(desiredFeatures, cluster.Status.AgentFeatures)
	repoChange := false
	if cluster.Spec.RancherKubernetesEngineConfig != nil {
		if cluster.Status.AppliedSpec.RancherKubernetesEngineConfig != nil {
			if len(cluster.Status.AppliedSpec.RancherKubernetesEngineConfig.PrivateRegistries) > 0 {
				desiredRepo := util.GetPrivateRepo(cluster)
				appliedRepo := &cluster.Status.AppliedSpec.RancherKubernetesEngineConfig.PrivateRegistries[0]

				if desiredRepo != nil && appliedRepo != nil && !reflect.DeepEqual(desiredRepo, appliedRepo) {
					repoChange = true
				}
				if (desiredRepo == nil && appliedRepo != nil) || (desiredRepo != nil && appliedRepo == nil) {
					repoChange = true
				}
			}
		}
	}
	if forceDeploy || imageChange || repoChange || agentFeaturesChanged {
		logrus.Infof("Redeploy Rancher Agents is needed for %s: forceDeploy=%v, agent/auth image changed=%v,"+
			" private repo changed=%v, agent features changed=%v", cluster.Name, forceDeploy, imageChange, repoChange,
			agentFeaturesChanged)
		logrus.Tracef("clusterDeploy: redeployAgent: cluster.Status.AgentImage: [%s], desiredAgent: [%s]", cluster.Status.AgentImage, desiredAgent)
		logrus.Tracef("clusterDeploy: redeployAgent: cluster.Status.AuthImage [%s], desiredAuth: [%s]", cluster.Status.AuthImage, desiredAuth)
		logrus.Tracef("clusterDeploy: redeployAgent: cluster.Status.AgentFeatures [%v], desiredFeatures: [%v]", cluster.Status.AgentFeatures, desiredFeatures)
		return true
	}

	na, ca := getAgentImages(cluster.Name)
	if (cluster.Status.AgentImage != na && cluster.Status.Driver == v32.ClusterDriverRKE) || cluster.Status.AgentImage != ca {
		// downstream agent does not match, kick a redeploy with settings agent
		logrus.Infof("clusterDeploy: redeployAgent: redeploy Rancher agents due to downstream agent image mismatch for [%s]: was [%s] and will be [%s]",
			cluster.Name, na, image.ResolveWithCluster(settings.AgentImage.Get(), cluster))
		clearAgentImages(cluster.Name)
		return true
	}

	// Taints/tolerations
	// Current taints are cached for comparison
	currentTaints := getCachedTaints(cluster.Name)
	logrus.Tracef("clusterDeploy: redeployAgent: cluster [%s] currentTaints: [%v]", cluster.Name, currentTaints)
	logrus.Tracef("clusterDeploy: redeployAgent: cluster [%s] desiredTaints: [%v]", cluster.Name, desiredTaints)
	toAdd, toDelete := taints.GetToDiffTaints(currentTaints, desiredTaints)
	// Any change to current triggers redeploy
	if len(toAdd) > 0 || len(toDelete) > 0 {
		logrus.Infof("clusterDeploy: redeployAgent: redeploy Rancher agents due to toleration mismatch for [%s], was [%v] and will be [%v]", cluster.Name, currentTaints, desiredTaints)
		// Clear cache to refresh
		clearControlPlaneTaints(cluster.Name)
		return true
	}

	if !reflect.DeepEqual(cluster.Spec.AgentEnvVars, cluster.Status.AppliedAgentEnvVars) {
		logrus.Infof("clusterDeploy: redeployAgent: redeploy Rancher agents due to agent env vars mismatched for [%s], was [%v] and will be [%v]", cluster.Name, cluster.Spec.AgentEnvVars, cluster.Status.AppliedAgentEnvVars)
		return true
	}

	logrus.Tracef("clusterDeploy: redeployAgent: returning false for redeployAgent")

	return false
}

func getDesiredImage(cluster *v3.Cluster) string {
	if cluster.Spec.AgentImageOverride != "" {
		return cluster.Spec.AgentImageOverride
	}

	return cluster.Spec.DesiredAgentImage
}

func (cd *clusterDeploy) deployAgent(cluster *v3.Cluster) error {
	if cluster.Spec.Internal {
		return nil
	}

	logrus.Tracef("clusterDeploy: deployAgent called for [%s]", cluster.Name)
	desiredAgent := getDesiredImage(cluster)
	if desiredAgent == "" || desiredAgent == "fixed" {
		desiredAgent = image.ResolveWithCluster(settings.AgentImage.Get(), cluster)
	}
	logrus.Tracef("clusterDeploy: deployAgent: desiredAgent is [%s] for cluster [%s]", desiredAgent, cluster.Name)

	var desiredAuth string
	if cluster.Spec.LocalClusterAuthEndpoint.Enabled {
		desiredAuth = cluster.Spec.DesiredAuthImage
		if desiredAuth == "" || desiredAuth == "fixed" {
			desiredAuth = image.ResolveWithCluster(settings.AuthImage.Get(), cluster)
		}
	}
	logrus.Tracef("clusterDeploy: deployAgent: desiredAuth is [%s] for cluster [%s]", desiredAuth, cluster.Name)

	desiredFeatures := map[string]bool{}

	logrus.Tracef("clusterDeploy: deployAgent: desiredFeatures is [%v] for cluster [%s]", desiredFeatures, cluster.Name)

	desiredTaints, err := cd.getControlPlaneTaints(cluster.Name)
	if err != nil {
		return err
	}
	logrus.Tracef("clusterDeploy: deployAgent: desiredTaints is [%v] for cluster [%s]", desiredTaints, cluster.Name)

	if !redeployAgent(cluster, desiredAgent, desiredAuth, desiredFeatures, desiredTaints) {
		return nil
	}

	kubeConfig, err := cd.getKubeConfig(cluster)
	if err != nil {
		return err
	}

	if _, err = v32.ClusterConditionAgentDeployed.Do(cluster, func() (runtime.Object, error) {
		yaml, err := cd.getYAML(cluster, desiredAgent, desiredAuth, desiredFeatures, desiredTaints)
		if err != nil {
			return cluster, err
		}
		logrus.Tracef("clusterDeploy: deployAgent: agent YAML: %v", string(yaml))
		var output []byte
		for i := 0; i < 5; i++ {
			// This will fail almost always the first time because when we create the namespace in the file it won't have privileges.
			// This allows for 5*5 seconds for the cluster to be ready to apply the agent YAML before erroring out
			logrus.Tracef("clusterDeploy: deployAgent: applying agent YAML for cluster [%s], try #%d: %v", cluster.Name, i+1, string(output))
			output, err = kubectl.Apply(yaml, kubeConfig)
			if err == nil {
				logrus.Tracef("clusterDeploy: deployAgent: successfully applied agent YAML for cluster [%s], try #%d", cluster.Name, i+1)
				break
			}
			logrus.Debugf("clusterDeploy: deployAgent: error while applying agent YAML for cluster [%s], try #%d", cluster.Name, i+1)
			time.Sleep(5 * time.Second)
		}
		if err != nil {
			return cluster, errors.WithMessage(types.NewErrors(err, errors.New(formatKubectlApplyOutput(string(output)))), "Error while applying agent YAML, it will be retried automatically")
		}
		v32.ClusterConditionAgentDeployed.Message(cluster, string(output))
		if !cluster.Spec.LocalClusterAuthEndpoint.Enabled && cluster.Status.AppliedSpec.LocalClusterAuthEndpoint.Enabled && cluster.Status.AuthImage != "" {
			output, err = kubectl.Delete([]byte(systemtemplate.AuthDaemonSet), kubeConfig)
		}
		if err != nil {
			logrus.Tracef("Output from kubectl delete kube-api-auth DaemonSet, output: %s, err: %v", string(output), err)
			// Ignore if the resource does not exist and it returns 'daemonsets.apps "kube-api-auth" not found'
			dsNotFoundError := "daemonsets.apps \"kube-api-auth\" not found"
			if !strings.Contains(string(output), dsNotFoundError) {
				return cluster, errors.WithMessage(types.NewErrors(err, errors.New(string(output))), "kubectl delete failed")
			}
			logrus.Debugf("Ignored '%s' error during delete kube-api-auth DaemonSet", dsNotFoundError)
		}
		if cluster.Status.Driver != v32.ClusterDriverRKE {
			if output, err = kubectl.Delete([]byte(systemtemplate.NodeAgentDaemonSet), kubeConfig); err != nil {
				logrus.Tracef("Output from kubectl delete cattle-node-agent DaemonSet, output: %s, err: %v", string(output), err)
				// Ignore if the resource does not exist and it returns 'daemonsets.apps "kube-api-auth" not found'
				dsNotFoundError := "daemonsets.apps \"cattle-node-agent\" not found"
				if !strings.Contains(string(output), dsNotFoundError) {
					return cluster, errors.WithMessage(types.NewErrors(err, errors.New(string(output))), "kubectl delete failed")
				}
				logrus.Debugf("Ignored '%s' error during delete cattle-node-agent DaemonSet", dsNotFoundError)
			}
		}
		v32.ClusterConditionAgentDeployed.Message(cluster, string(output))
		return cluster, nil
	}); err != nil {
		return err
	}

	if err = cd.cacheAgentImages(cluster.Name); err != nil {
		return err
	}

	cluster.Status.AgentImage = desiredAgent
	cluster.Status.AgentFeatures = desiredFeatures
	if cluster.Spec.DesiredAgentImage == "fixed" {
		cluster.Spec.DesiredAgentImage = desiredAgent
	}
	cluster.Status.AuthImage = desiredAuth
	if cluster.Spec.DesiredAuthImage == "fixed" {
		cluster.Spec.DesiredAuthImage = desiredAuth
	}
	if cluster.Annotations[AgentForceDeployAnn] == "true" {
		cluster.Annotations[AgentForceDeployAnn] = "false"
	}
	cluster.Status.AppliedAgentEnvVars = cluster.Spec.AgentEnvVars

	return nil
}

func (cd *clusterDeploy) setNetworkPolicyAnn(cluster *v3.Cluster) error {
	if cluster.Spec.EnableNetworkPolicy != nil {
		return nil
	}
	// set current state for upgraded canal clusters
	if cluster.Spec.RancherKubernetesEngineConfig != nil &&
		cluster.Spec.RancherKubernetesEngineConfig.Network.Plugin == "canal" {
		enableNetworkPolicy := true
		cluster.Spec.EnableNetworkPolicy = &enableNetworkPolicy
		cluster.Annotations["networking.management.cattle.io/enable-network-policy"] = "true"
	}
	return nil
}

func (cd *clusterDeploy) getKubeConfig(cluster *v3.Cluster) (*clientcmdapi.Config, error) {
	logrus.Tracef("clusterDeploy: getKubeConfig called for cluster [%s]", cluster.Name)
	user, err := cd.systemAccountManager.GetSystemUser(cluster.Name)
	if err != nil {
		return nil, err
	}

	token, err := cd.userManager.EnsureToken("agent-"+user.Name, "token for agent deployment", "agent", user.Name)
	if err != nil {
		return nil, err
	}

	return cd.clusterManager.KubeConfig(cluster.Name, token), nil
}

func (cd *clusterDeploy) getYAML(cluster *v3.Cluster, agentImage, authImage string, features map[string]bool, taints []corev1.Taint) ([]byte, error) {
	logrus.Tracef("clusterDeploy: getYAML: Desired agent image is [%s] for cluster [%s]", agentImage, cluster.Name)
	logrus.Tracef("clusterDeploy: getYAML: Desired auth image is [%s] for cluster [%s]", authImage, cluster.Name)
	logrus.Tracef("clusterDeploy: getYAML: Desired features are [%v] for cluster [%s]", features, cluster.Name)
	logrus.Tracef("clusterDeploy: getYAML: Desired taints are [%v] for cluster [%s]", taints, cluster.Name)

	token, err := cd.systemAccountManager.GetOrCreateSystemClusterToken(cluster.Name)
	if err != nil {
		return nil, err
	}

	url := settings.ServerURL.Get()
	if url == "" {
		cd.clusters.Controller().EnqueueAfter("", cluster.Name, time.Second)
		return nil, fmt.Errorf("waiting for server-url setting to be set")
	}

	buf := &bytes.Buffer{}
	err = systemtemplate.SystemTemplate(buf, agentImage, authImage, cluster.Name, token, url, cluster.Spec.WindowsPreferedCluster,
		cluster, features, taints)

	return buf.Bytes(), err
}

func (cd *clusterDeploy) getClusterAgentImage(name string) (string, error) {
	uc, err := cd.clusterManager.UserContext(name)
	if err != nil {
		return "", err
	}

	d, err := uc.Apps.Deployments("cattle-system").Get("cattle-cluster-agent", v1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return "", err
		}
		return "", nil
	}

	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "cluster-register" {
			return c.Image, nil
		}
	}

	return "", nil
}

func (cd *clusterDeploy) getNodeAgentImage(name string) (string, error) {
	uc, err := cd.clusterManager.UserContext(name)
	if err != nil {
		return "", err
	}

	ds, err := uc.Apps.DaemonSets("cattle-system").Get("cattle-node-agent", v1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return "", err
		}
		return "", nil
	}

	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name == "agent" {
			return c.Image, nil
		}
	}

	return "", nil
}

func (cd *clusterDeploy) cacheAgentImages(name string) error {
	na, err := cd.getNodeAgentImage(name)
	if err != nil {
		return err
	}

	ca, err := cd.getClusterAgentImage(name)
	if err != nil {
		return err
	}

	agentImagesMutex.Lock()
	defer agentImagesMutex.Unlock()
	agentImages[nodeImage][name] = na
	agentImages[clusterImage][name] = ca
	return nil
}

func agentImagesCached(name string) bool {
	na, ca := getAgentImages(name)
	return na != "" && ca != ""
}

func controlPlaneTaintsCached(name string) bool {
	controlPlaneTaintsMutex.RLock()
	defer controlPlaneTaintsMutex.RUnlock()
	if _, ok := controlPlaneTaints[name]; ok {
		return true
	}
	return false
}

func getAgentImages(name string) (string, string) {
	agentImagesMutex.RLock()
	defer agentImagesMutex.RUnlock()
	return agentImages[nodeImage][name], agentImages[clusterImage][name]
}

func getCachedTaints(name string) []corev1.Taint {
	controlPlaneTaintsMutex.RLock()
	defer controlPlaneTaintsMutex.RUnlock()
	if _, ok := controlPlaneTaints[name]; ok {
		return controlPlaneTaints[name]
	}
	return nil
}

func clearAgentImages(name string) {
	logrus.Tracef("clusterDeploy: clearAgentImages called for [%s]", name)
	agentImagesMutex.Lock()
	defer agentImagesMutex.Unlock()
	delete(agentImages[nodeImage], name)
	delete(agentImages[clusterImage], name)
}

func clearControlPlaneTaints(name string) {
	logrus.Tracef("clusterDeploy: clearControlPlaneTaints called for [%s]", name)
	controlPlaneTaintsMutex.Lock()
	defer controlPlaneTaintsMutex.Unlock()
	delete(controlPlaneTaints, name)
}

func (cd *clusterDeploy) cacheControlPlaneTaints(name string) error {
	taints, err := cd.getControlPlaneTaints(name)
	if err != nil {
		return err
	}

	controlPlaneTaintsMutex.Lock()
	defer controlPlaneTaintsMutex.Unlock()
	controlPlaneTaints[name] = taints
	return nil
}

func (cd *clusterDeploy) getControlPlaneTaints(name string) ([]corev1.Taint, error) {
	var allTaints []corev1.Taint
	var controlPlaneLabelFound bool
	nodes, err := cd.nodeLister.List(name, labels.Everything())
	if err != nil {
		return nil, err
	}
	logrus.Debugf("clusterDeploy: getControlPlaneTaints: Length of nodes for cluster [%s] is: %d", name, len(nodes))

	for _, node := range nodes {
		controlPlaneLabelFound = false
		// Filtering nodes for controlplane nodes based on labels
		for controlPlaneLabelKey, controlPlaneLabelValue := range controlPlaneLabels {
			if labelValue, ok := node.Status.NodeLabels[controlPlaneLabelKey]; ok {
				logrus.Tracef("clusterDeploy: getControlPlaneTaints: node [%s] has label key [%s]", node.Status.NodeName, controlPlaneLabelKey)
				if labelValue == controlPlaneLabelValue {
					logrus.Tracef("clusterDeploy: getControlPlaneTaints: node [%s] has label key [%s] and label value [%s]", node.Status.NodeName, controlPlaneLabelKey, controlPlaneLabelValue)
					controlPlaneLabelFound = true
					break
				}
			}
		}
		if controlPlaneLabelFound {
			toAdd, _ := taints.GetToDiffTaints(allTaints, node.Spec.InternalNodeSpec.Taints)
			for _, taintStr := range toAdd {
				if !strings.HasPrefix(taintStr.Key, "node.kubernetes.io") {
					logrus.Debugf("clusterDeploy: getControlPlaneTaints: toAdd: %v", toAdd)
					allTaints = append(allTaints, taintStr)
					continue
				}
				logrus.Tracef("clusterDeploy: getControlPlaneTaints: skipping taint [%v] because its k8s internal", taintStr)
			}
		}
	}
	logrus.Debugf("clusterDeploy: getControlPlaneTaints: allTaints: %v", allTaints)

	return allTaints, nil
}

func formatKubectlApplyOutput(log string) string {
	// Strip newlines to compact output
	log = strings.Replace(log, "\n", " ", -1)
	// Strip token from output
	tokenRegex := regexp.MustCompile(`^.*?\"token\":\"(.*?)\"`)
	token := tokenRegex.FindStringSubmatch(log)
	if len(token) == 2 {
		log = strings.Replace(log, token[1], "REDACTED", 1)
	}
	return log
}