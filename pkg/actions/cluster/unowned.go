package cluster

import (
	"fmt"
	"time"

	"github.com/weaveworks/eksctl/pkg/kubernetes"

	iamoidc "github.com/weaveworks/eksctl/pkg/iam/oidc"

	"github.com/weaveworks/eksctl/pkg/cfn/manager"

	"github.com/kris-nova/logger"

	awseks "github.com/aws/aws-sdk-go/service/eks"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/eks"
)

type UnownedCluster struct {
	cfg *api.ClusterConfig
	ctl *eks.ClusterProvider
}

func NewUnownedCluster(cfg *api.ClusterConfig, ctl *eks.ClusterProvider) (*UnownedCluster, error) {
	return &UnownedCluster{
		cfg: cfg,
		ctl: ctl,
	}, nil
}

func (c *UnownedCluster) Upgrade(dryRun bool) error {
	_, err := upgrade(c.cfg, c.ctl, dryRun)
	return err
}

func (c *UnownedCluster) Delete(waitTimeout time.Duration, wait bool) error {
	clusterName := c.cfg.Metadata.Name

	if err := c.checkClusterExists(clusterName); err != nil {
		return err
	}

	clientSet, err := c.ctl.NewStdClientSet(c.cfg)
	if err != nil {
		return err
	}

	if err := deleteCommon(c.cfg, c.ctl, clientSet, waitTimeout); err != nil {
		return err
	}

	nodegroups, err := c.ctl.Provider.EKS().ListNodegroups(&awseks.ListNodegroupsInput{
		ClusterName: &clusterName,
	})

	if err != nil {
		return err
	}

	for _, nodeGroupName := range nodegroups.Nodegroups {
		out, err := c.ctl.Provider.EKS().DeleteNodegroup(&awseks.DeleteNodegroupInput{
			ClusterName:   &clusterName,
			NodegroupName: nodeGroupName,
		})

		if err != nil {
			return err
		}
		logger.Info("initiated deletion of nodegroup %q", *nodeGroupName)

		if out != nil {
			logger.Debug("delete nodegroup %q response: %s", *nodeGroupName, out.String())
		}
	}

	err = c.waitForNodegroupsDeletion(clusterName, waitTimeout)

	if err != nil {
		return err
	}

	if err := c.deleteIAMAndOIDC(wait, kubernetes.NewCachedClientSet(clientSet)); err != nil {
		return err
	}

	out, err := c.ctl.Provider.EKS().DeleteCluster(&awseks.DeleteClusterInput{
		Name: &clusterName,
	})

	if err != nil {
		return err
	}

	logger.Info("initiated deletion of cluster %q", clusterName)
	if out != nil {
		logger.Debug("delete cluster response: %s", out.String())
	}

	if wait {
		return c.waitForClusterDeletion(clusterName, waitTimeout)
	}
	logger.Info("to see the status of the deletion run `eksctl get cluster --name %s --region %s`", clusterName, c.cfg.Metadata.Region)
	return nil
}

func (c *UnownedCluster) checkClusterExists(clusterName string) error {
	out, err := c.ctl.Provider.EKS().ListClusters(&awseks.ListClustersInput{})
	if err != nil {
		return err
	}
	for _, cluster := range out.Clusters {
		if *cluster == clusterName {
			return nil
		}
	}

	return fmt.Errorf("cluster %q not found", clusterName)
}

func (c *UnownedCluster) deleteIAMAndOIDC(wait bool, clientSetGetter kubernetes.ClientSetGetter) error {
	clusterOperable, _ := c.ctl.CanOperate(c.cfg)

	var oidc *iamoidc.OpenIDConnectManager
	var err error
	stackManager := c.ctl.NewStackManager(c.cfg)

	oidcSupported := true
	if clusterOperable {
		oidc, err = c.ctl.NewOpenIDConnectManager(c.cfg)
		if err != nil {
			if _, ok := err.(*eks.UnsupportedOIDCError); !ok {
				return err
			}
			oidcSupported = false
		}
	}

	deleteOIDCProvider := clusterOperable && oidcSupported

	tasks := &manager.TaskTree{Parallel: false}

	if deleteOIDCProvider {
		serviceAccountAndOIDCTasks, err := stackManager.NewTasksToDeleteOIDCProviderWithIAMServiceAccounts(oidc, clientSetGetter)
		if err != nil {
			return err
		}

		if serviceAccountAndOIDCTasks.Len() > 0 {
			serviceAccountAndOIDCTasks.IsSubTask = true
			tasks.Append(serviceAccountAndOIDCTasks)
		}
	}

	deleteAddonIAMtasks, err := stackManager.NewTaskToDeleteAddonIAM(wait)
	if err != nil {
		return err
	}

	if deleteAddonIAMtasks.Len() > 0 {
		deleteAddonIAMtasks.IsSubTask = true
		tasks.Append(deleteAddonIAMtasks)
	}

	if tasks.Len() == 0 {
		logger.Warning("no IAM and OIDC resources were found for %q", c.cfg.Metadata.Name)
		return nil
	}

	logger.Info(tasks.Describe())
	if errs := tasks.DoAllSync(); len(errs) > 0 {
		return handleErrors(errs, "deleting cluster IAM and OIDC")
	}

	logger.Info("all IAM and OIDC resources were deleted")
	return nil
}

func (c *UnownedCluster) waitForClusterDeletion(clusterName string, waitTimeout time.Duration) error {
	condition := func() (bool, error) {
		clusterDeleted := true
		clusters, err := c.ctl.Provider.EKS().ListClusters(&awseks.ListClustersInput{})
		if err != nil {
			return false, err
		}
		for _, cluster := range clusters.Clusters {
			if *cluster == clusterName {
				clusterDeleted = false
			}
		}
		if clusterDeleted {
			logger.Info("cluster %q successfully deleted", clusterName)
			return true, nil
		}

		cluster, err := c.ctl.Provider.EKS().DescribeCluster(&awseks.DescribeClusterInput{
			Name: &clusterName,
		})

		if err == nil {
			logger.Info("waiting for cluster %q to be deleted, current status: %q", clusterName, *cluster.Cluster.Status)
		} else {
			logger.Debug("failed to get cluster status %v", err)
			logger.Info("waiting for cluster %q to be deleted")
		}
		return false, nil
	}
	return c.wait(waitTimeout, fmt.Errorf("timed out waiting for cluster %q  after %s", clusterName, waitTimeout), condition)
}

func (c *UnownedCluster) waitForNodegroupsDeletion(clusterName string, waitTimeout time.Duration) error {
	condition := func() (bool, error) {
		nodeGroups, err := c.ctl.Provider.EKS().ListNodegroups(&awseks.ListNodegroupsInput{
			ClusterName: &clusterName,
		})
		if err != nil {
			return false, err
		}
		if len(nodeGroups.Nodegroups) == 0 {
			logger.Info("all nodegroups for cluster %q successfully deleted", clusterName)
			return true, nil
		}

		logger.Info("waiting for nodegroups to be deleted, %d remaining", len(nodeGroups.Nodegroups))
		return false, nil
	}

	return c.wait(waitTimeout, fmt.Errorf("timed out waiting for nodegroup deletion after %s", waitTimeout), condition)
}

func (c *UnownedCluster) wait(waitTimeout time.Duration, returnErr error, condition func() (bool, error)) error {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	timer := time.NewTimer(waitTimeout)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			finished, err := condition()
			if err != nil {
				return err
			}

			if finished {
				return nil
			}

		case <-timer.C:
			return returnErr
		}
	}
}
