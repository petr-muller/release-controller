package main

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
	prowapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	config "k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/cron"
	"k8s.io/test-infra/prow/pjutil"
)

type PeriodicWithRelease struct {
	Periodic    *config.Periodic
	Release     *Release
	Upgrade     bool
	UpgradeFrom string
}

func (c *Controller) syncPeriodicJobs(prowInformers cache.SharedIndexInformer, stopCh <-chan struct{}) {
	prowIndex := prowInformers.GetIndexer()
	cache.WaitForCacheSync(stopCh, prowInformers.HasSynced)
	cr := cron.New()
	cr.Start()
	wait.Until(func() {
		imagestreams, err := c.releaseLister.List(labels.Everything())
		if err != nil {
			klog.Errorf("failed to get list of imagestreams: %v", err)
			return
		}
		cfg := c.prowConfigLoader.Config()
		if cfg == nil {
			klog.Errorf("the prow config is not valid: no prow jobs have been defined")
			return
		}
		releasePeriodics := make(map[string]PeriodicWithRelease)
		// to reuse cron code from k8s test-infra, we can create a fake prow Config that just has just the periodics specified in the release configs
		cronConfig := &config.Config{}
		for _, is := range imagestreams {
			r, ok, err := c.releaseDefinition(is)
			if err != nil || !ok {
				continue
			}
			for _, periodic := range r.Config.Periodic {
				periodicConfig, ok := hasProwJob(cfg, periodic.ProwJob.Name)
				if !ok {
					klog.Errorf("the prow job %s is not valid: no job with that name", periodic.ProwJob.Name)
					continue
				}
				if err := validateProwJob(periodicConfig); err != nil {
					klog.Errorf("the prowjob %s is not valid: %v", periodic.ProwJob.Name, err)
					continue
				}
				// create unique job name based on release; this prevents issues where the same base job is specified
				// by 2 different releases (for example nightly vs ci) or when a job gets runs by horologium
				jobName := fmt.Sprintf("%s-%s-periodic", periodic.ProwJob.Name, r.Config.Name)
				// make new copy of periodicConfig so we can update the name
				newPeriodicConfig := *periodicConfig
				newPeriodicConfig.Name = jobName
				releasePeriodics[jobName] = PeriodicWithRelease{
					Periodic:    &newPeriodicConfig,
					Release:     r,
					Upgrade:     periodic.Upgrade,
					UpgradeFrom: periodic.UpgradeFrom,
				}
				cronConfig.Periodics = append(cronConfig.Periodics, newPeriodicConfig)
			}
		}
		// update cron
		if err := cr.SyncConfig(cronConfig); err != nil {
			klog.Errorf("Error syncing cron jobs: %v", err)
		}

		cronTriggers := sets.NewString()
		for _, job := range cr.QueuedJobs() {
			cronTriggers.Insert(job)
		}

		// get current prowjobs; returned as []interface, and thus must be converted to unstructured and then periodics
		jobInterfaces := prowIndex.List()
		jobs := []prowapi.ProwJob{}
		for _, item := range jobInterfaces {
			unstructuredJob, ok := item.(*unstructured.Unstructured)
			if !ok {
				klog.Warning("job interface from prow informer index list could not be cast to unstructured")
				continue
			}
			prowjob := prowapi.ProwJob{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredJob.UnstructuredContent(), &prowjob); err != nil {
				klog.Errorf("failed to convert unstructured prowjob to prowjob type object: %v", err)
				continue
			}
			jobs = append(jobs, prowjob)
		}
		latestJobs := pjutil.GetLatestProwJobs(jobs, prowapi.PeriodicJob)

		var errs []error
		for _, p := range cronConfig.Periodics {
			j, previousFound := latestJobs[p.Name]
			if p.Cron == "" {
				shouldTrigger := j.Complete() && time.Now().Sub(j.Status.StartTime.Time) > p.GetInterval()
				if !previousFound || shouldTrigger {
					err := c.createProwJobFromPeriodicWithRelease(releasePeriodics[p.Name])
					if err != nil {
						errs = append(errs, err)
					}
				}
			} else if cronTriggers.Has(p.Name) {
				shouldTrigger := j.Complete()
				if !previousFound || shouldTrigger {
					err := c.createProwJobFromPeriodicWithRelease(releasePeriodics[p.Name])
					if err != nil {
						errs = append(errs, err)
					}
				}
			}
		}

		if len(errs) > 0 {
			klog.Errorf("failed to create %d periodic prowjobs: %v", len(errs), errs)
		}
	}, 2*time.Minute, stopCh)
}

func (c *Controller) createProwJobFromPeriodicWithRelease(periodicWithRelease PeriodicWithRelease) error {
	// get release info
	release := periodicWithRelease.Release
	acceptedTags := sortedRawReleaseTags(release, releasePhaseAccepted)
	if len(acceptedTags) == 0 {
		return fmt.Errorf("no accepted tags found for release %s", release.Config.Name)
	}
	latestTag := acceptedTags[0]
	mirror, err := c.getMirror(release, latestTag.Name)
	if err != nil {
		return fmt.Errorf("failed to get mirror for release %s tag %s: %v", release.Config.Name, latestTag.Name, err)
	}
	var previousTag, previousReleasePullSpec string
	if periodicWithRelease.Upgrade {
		previousTag, previousReleasePullSpec, err = c.getUpgradeTagAndPullSpec(release, latestTag, periodicWithRelease.Periodic.Name, periodicWithRelease.UpgradeFrom)
		if err != nil {
			return fmt.Errorf("failed to get previous release spec and tag for release %s tag %s: %v", release.Config.Name, latestTag.Name, err)
		}
	}
	spec := pjutil.PeriodicSpec(*periodicWithRelease.Periodic)
	ok, err := addReleaseEnvToProwJobSpec(&spec, release, mirror, latestTag, previousReleasePullSpec)
	if err != nil || !ok {
		return fmt.Errorf("failed to add release env to periodic %s: %v", periodicWithRelease.Periodic.Name, err)
	}
	prowJob := pjutil.NewProwJob(spec, periodicWithRelease.Periodic.Labels, periodicWithRelease.Periodic.Annotations)
	prowJob.Labels[releaseAnnotationVerify] = "true"
	prowJob.Annotations[releaseAnnotationSource] = fmt.Sprintf("%s/%s", release.Source.Namespace, release.Source.Name)
	prowJob.Annotations[releaseAnnotationToTag] = latestTag.Name
	if periodicWithRelease.Upgrade && len(previousTag) > 0 {
		prowJob.Annotations[releaseAnnotationFromTag] = previousTag
	}

	_, err = c.prowClient.Create(objectToUnstructured(&prowJob), metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create periodic prowjob %s: %v", periodicWithRelease.Periodic.Name, err)
	}
	klog.V(2).Infof("Created new prow job %s", prowJob.Name)
	return nil
}
