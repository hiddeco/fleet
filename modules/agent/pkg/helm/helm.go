package helm

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rancher/fleet/modules/agent/pkg/deployer"
	fleet "github.com/rancher/fleet/pkg/apis/fleet.cattle.io/v1alpha1"
	"github.com/rancher/fleet/pkg/kustomize"
	"github.com/rancher/fleet/pkg/manifest"
	"github.com/rancher/fleet/pkg/render"
	"github.com/rancher/wrangler/pkg/apply"
	"github.com/rancher/wrangler/pkg/kv"
	"github.com/rancher/wrangler/pkg/name"
	"github.com/rancher/wrangler/pkg/yaml"
	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

type helm struct {
	cfg action.Configuration
}

func NewHelm(namespace string, getter genericclioptions.RESTClientGetter) (deployer.Deployer, error) {
	h := &helm{}
	if err := h.cfg.Init(getter, namespace, "secrets", logrus.Infof); err != nil {
		return nil, err
	}
	h.cfg.Releases.MaxHistory = 5
	return h, nil
}

func mergeMaps(base, other map[string]string) map[string]string {
	result := map[string]string{}
	for k, v := range base {
		result[k] = v
	}
	for k, v := range other {
		result[k] = v
	}
	return result
}

type postRender struct {
	bundleID string
	manifest *manifest.Manifest
	opts     fleet.BundleDeploymentOptions
}

func (p *postRender) Run(renderedManifests *bytes.Buffer) (modifiedManifests *bytes.Buffer, err error) {
	objs, err := yaml.ToObjects(renderedManifests)
	if err != nil {
		return nil, err
	}

	newObjs, processed, err := kustomize.Process(p.manifest, renderedManifests.Bytes(), p.opts.KustomizeDir)
	if err != nil {
		return nil, err
	}
	if processed {
		objs = newObjs
	}

	labels, annotations, err := apply.GetLabelsAndAnnotations(name.SafeConcatName("fleet", p.bundleID), nil)
	if err != nil {
		return nil, err
	}

	for _, obj := range objs {
		meta, err := meta.Accessor(obj)
		if err != nil {
			return nil, err
		}
		meta.SetLabels(mergeMaps(meta.GetLabels(), labels))
		meta.SetAnnotations(mergeMaps(meta.GetAnnotations(), annotations))
	}

	data, err := yaml.ToBytes(objs)
	return bytes.NewBuffer(data), err
}

func (h *helm) Deploy(bundleID string, manifest *manifest.Manifest, options fleet.BundleDeploymentOptions) (*deployer.Resources, error) {
	tar, err := render.ToChart(bundleID, manifest)
	if err != nil {
		return nil, err
	}

	chart, err := loader.LoadArchive(tar)
	if err != nil {
		return nil, err
	}

	if chart.Metadata.Annotations == nil {
		chart.Metadata.Annotations = map[string]string{}
	}
	chart.Metadata.Annotations["bundleID"] = bundleID

	if _, err := h.install(bundleID, chart, options, true); err != nil {
		return nil, err
	}

	release, err := h.install(bundleID, chart, options, false)
	if err != nil {
		return nil, err
	}

	return releaseToResources(release)
}

func (h *helm) mustUninstall(bundleID string) (bool, error) {
	r, err := h.cfg.Releases.Last(bundleID)
	if err != nil {
		return false, nil
	}
	return r.Info.Status == release.StatusUninstalling, err
}

func (h *helm) mustInstall(bundleID string) (bool, error) {
	_, err := h.cfg.Releases.Deployed(bundleID)
	if err != nil && strings.Contains(err.Error(), "has no deployed releases") {
		return true, nil
	}
	return false, err
}

func getOpts(options fleet.BundleDeploymentOptions) (map[string]interface{}, time.Duration, string) {
	vals := map[string]interface{}{}
	if options.Values != nil {
		vals = options.Values.Object
	}

	timeout := 10 * time.Minute
	if options.TimeoutSeconds > 0 {
		timeout = time.Second * time.Duration(options.TimeoutSeconds)
	}

	if options.DefaultNamespace == "" {
		options.DefaultNamespace = "default"
	}

	return vals, timeout, options.DefaultNamespace
}

func (h *helm) install(bundleID string, chart *chart.Chart, options fleet.BundleDeploymentOptions, dryRun bool) (*release.Release, error) {
	vals, timeout, namespace := getOpts(options)

	uninstall, err := h.mustUninstall(bundleID)
	if err != nil {
		return nil, err
	}
	if uninstall {
		if err := h.delete(bundleID, options, dryRun); err != nil {
			return nil, err
		}
		if dryRun {
			return nil, nil
		}
	}

	install, err := h.mustInstall(bundleID)
	if err != nil {
		return nil, err
	}

	if install {
		u := action.NewInstall(&h.cfg)
		u.Adopt = true
		u.Replace = true
		u.Wait = true
		u.ReleaseName = bundleID
		u.CreateNamespace = true
		u.Namespace = namespace
		u.Timeout = timeout
		u.DryRun = dryRun
		u.PostRenderer = &postRender{
			bundleID: bundleID,
		}
		return u.Run(chart, vals)
	}

	u := action.NewUpgrade(&h.cfg)
	u.Adopt = true
	u.Namespace = namespace
	u.Timeout = timeout
	u.Atomic = true
	u.DryRun = dryRun
	u.PostRenderer = &postRender{
		bundleID: bundleID,
	}
	return u.Run(bundleID, chart, vals)
}

func (h *helm) ListDeployments() ([]string, error) {
	list := action.NewList(&h.cfg)
	list.All = true
	releases, err := list.Run()
	if err != nil {
		return nil, err
	}

	var (
		seen   = map[string]bool{}
		result []string
	)

	for _, release := range releases {
		d := release.Chart.Metadata.Annotations["bundleID"]
		if d != "" && !seen[d] {
			result = append(result, d)
			seen[d] = true
		}
	}

	return result, nil
}

func (h *helm) Resources(deploymentID, resourcesID string) (*deployer.Resources, error) {
	hist := action.NewHistory(&h.cfg)

	releases, err := hist.Run(deploymentID)
	if err != nil {
		return nil, err
	}

	releaseName, versionStr := kv.Split(resourcesID, ":")
	version, _ := strconv.Atoi(versionStr)

	for _, release := range releases {
		if release.Name == releaseName && release.Version == version {
			return releaseToResources(release)
		}
	}

	return &deployer.Resources{}, nil
}

func (h *helm) Delete(bundleID string) error {
	return h.delete(bundleID, fleet.BundleDeploymentOptions{}, false)
}

func (h *helm) delete(bundleID string, options fleet.BundleDeploymentOptions, dryRun bool) error {
	_, timeout, _ := getOpts(options)

	u := action.NewUninstall(&h.cfg)
	u.DryRun = dryRun
	u.Timeout = timeout

	_, err := u.Run(bundleID)
	return err
}

func releaseToResources(release *release.Release) (*deployer.Resources, error) {
	var (
		err error
	)
	resources := &deployer.Resources{
		DefaultNamespace: release.Namespace,
		ID:               fmt.Sprintf("%s:%d", release.Name, release.Version),
	}

	resources.Objects, err = yaml.ToObjects(bytes.NewBufferString(release.Manifest))
	return resources, err
}