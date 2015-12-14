package gcloudcleanup

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"

	"google.golang.org/api/compute/v1"
)

type instanceCleaner struct {
	cs  *compute.Service
	log *logrus.Entry

	projectID   string
	rateLimiter *time.Ticker
	filters     []string

	noop bool

	CutoffTime time.Time
}

type instanceDeletionRequest struct {
	Instance *compute.Instance
	Reason   string
}

func newInstanceCleaner(cs *compute.Service, log *logrus.Logger,
	rlTick time.Duration, cutoffTime time.Time,
	projectID string, filters []string, noop bool) *instanceCleaner {

	return &instanceCleaner{
		cs:  cs,
		log: log.WithField("component", "instance_cleaner"),

		projectID:   projectID,
		rateLimiter: time.NewTicker(rlTick),
		filters:     filters,

		noop: noop,

		CutoffTime: cutoffTime,
	}
}

func (ic *instanceCleaner) Run() error {
	ic.log.WithFields(logrus.Fields{
		"project":     ic.projectID,
		"cutoff_time": ic.CutoffTime.Format(time.RFC3339),
		"filters":     strings.Join(ic.filters, ","),
	}).Info("running instance cleanup")

	instChan := make(chan *instanceDeletionRequest)
	errChan := make(chan error)

	go ic.fetchInstancesToDelete(instChan, errChan)
	go func() {
		for err := range errChan {
			if err == nil {
				continue
			}
			ic.log.WithField("err", err).Warn("error during instance fetch")
		}
	}()

	nDeleted := 0

	for req := range instChan {
		if req == nil {
			break
		}

		err := ic.deleteInstance(req.Instance)

		if err != nil {
			ic.log.WithFields(logrus.Fields{
				"err":      err,
				"instance": req.Instance.Name,
			}).Warn("failed to delete instance")
		}

		nDeleted++

		ic.log.WithFields(logrus.Fields{
			"instance": req.Instance.Name,
			"reason":   req.Reason,
		}).Info("deleted")
	}

	ic.l2met("measure#instances.deleted", nDeleted, "done running instance cleanup")

	return nil
}

func (ic *instanceCleaner) fetchInstancesToDelete(instChan chan *instanceDeletionRequest, errChan chan error) {
	listCall := ic.cs.Instances.AggregatedList(ic.projectID)
	for _, filter := range ic.filters {
		listCall.Filter(filter)
	}

	pageTok := ""
	statusCounts := map[string]int{}
	nInstances := 0

	for {
		if pageTok != "" {
			listCall.PageToken(pageTok)
		}

		ic.apiRateLimit()
		ic.log.WithField("page_token", pageTok).Debug("fetching instances aggregated list")
		resp, err := listCall.Do()

		if err != nil {
			errChan <- err
			continue
		}

		ic.log.WithField("zones", len(resp.Items)).Debug("checking aggregated instance results")

		for zone, list := range resp.Items {
			ic.log.WithFields(logrus.Fields{
				"zone":      zone,
				"instances": len(list.Instances),
			}).Debug("checking instance results in zone")

			for _, inst := range list.Instances {
				nInstances++

				log := ic.log.WithFields(logrus.Fields{
					"instance": inst.Name,
				})

				if _, ok := statusCounts[inst.Status]; !ok {
					statusCounts[inst.Status] = 0
				}

				statusCounts[inst.Status]++

				if inst.Status == "TERMINATED" {
					log.WithFields(logrus.Fields{
						"status": inst.Status,
					}).Debug("sending instance for deletion")

					instChan <- &instanceDeletionRequest{Instance: inst, Reason: "terminated"}
					continue
				}

				ts, err := time.Parse(time.RFC3339, inst.CreationTimestamp)

				if err != nil {
					log.WithField("err", err).Warn("failed to parse creation timestamp")
					continue
				}

				ts = ts.UTC()

				log.WithFields(logrus.Fields{
					"orig":   inst.CreationTimestamp,
					"parsed": ts.Format(time.RFC3339),
				}).Debug("parsed and adjusted creation timestamp")

				if ts.Before(ic.CutoffTime) {
					log.WithFields(logrus.Fields{
						"created": ts.Format(time.RFC3339),
						"cutoff":  ic.CutoffTime.Format(time.RFC3339),
					}).Debug("sending instance for deletion")

					instChan <- &instanceDeletionRequest{Instance: inst, Reason: "stale"}
					continue
				}

				log.Debug("skipping instance")
			}
		}

		if resp.NextPageToken == "" {
			ic.log.Debug("no next page, breaking out of loop")
			break
		}

		ic.log.Debug("continuing to next page")
		pageTok = resp.NextPageToken
	}

	for status, count := range statusCounts {
		key := fmt.Sprintf("gauge#instances.status.%s", status)
		ic.l2met(key, count, "counted instances with status")
	}

	ic.l2met("gauge#instances.count", nInstances, "done checking all instances")
	instChan <- nil
	errChan <- nil
}

func (ic *instanceCleaner) deleteInstance(inst *compute.Instance) error {
	if ic.noop {
		ic.log.WithField("instance", inst.Name).Debug("not really deleting image")
		return nil
	}

	ic.apiRateLimit()
	_, err := ic.cs.Instances.Delete(ic.projectID, filepath.Base(inst.Zone), inst.Name).Do()
	return err
}

func (ic *instanceCleaner) l2met(name string, n int, msg string) {
	ic.log.WithField(name, n).Info(msg)
}

func (ic *instanceCleaner) apiRateLimit() {
	ic.log.Debug("waiting for rate limiter tick")
	<-ic.rateLimiter.C
}