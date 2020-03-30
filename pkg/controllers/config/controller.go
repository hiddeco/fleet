package config

import (
	"context"

	"github.com/rancher/fleet/pkg/config"
	corecontrollers "github.com/rancher/wrangler-api/pkg/generated/controllers/core/v1"
	v1 "k8s.io/api/core/v1"
)

func Register(ctx context.Context,
	namespace string,
	cm corecontrollers.ConfigMapController) error {

	cm.OnChange(ctx, "global-config", func(_ string, configMap *v1.ConfigMap) (*v1.ConfigMap, error) {
		return reloadConfig(namespace, configMap)
	})

	return nil
}

func reloadConfig(namespace string, configMap *v1.ConfigMap) (*v1.ConfigMap, error) {
	if configMap == nil {
		return nil, nil
	}

	if configMap.Name != config.ManagerConfigName ||
		configMap.Namespace != namespace {
		return configMap, nil
	}

	cfg, err := config.ReadConfig(configMap)
	if err != nil {
		return configMap, err
	}

	return configMap, config.Set(cfg)
}