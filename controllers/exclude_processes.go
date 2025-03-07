/*
 * exclude_processes.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2019-2021 Apple Inc. and the FoundationDB project authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package controllers

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/FoundationDB/fdb-kubernetes-operator/pkg/fdbstatus"
	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/api/v1beta2"
)

// ignoreMissingProcessDuration defines the duration a Process Group must have the MissingProcess condition to be
// ignored in the exclusion check and let the exclusions potentially move forward.
// We should consider to make this configurable in the long term.
const ignoreMissingProcessDuration = 5 * time.Minute

// excludeProcesses provides a reconciliation step for excluding processes from
// the database.
type excludeProcesses struct{}

// reconcile runs the reconciler's work.
func (e excludeProcesses) reconcile(_ context.Context, r *FoundationDBClusterReconciler, cluster *fdbv1beta2.FoundationDBCluster, status *fdbv1beta2.FoundationDBStatus, logger logr.Logger) *requeue {
	adminClient, err := r.getDatabaseClientProvider().GetAdminClient(cluster, r)
	if err != nil {
		return &requeue{curError: err}
	}
	defer adminClient.Close()

	// If the status is not cached, we have to fetch it.
	if status == nil {
		status, err = adminClient.GetStatus()
		if err != nil {
			return &requeue{curError: err}
		}
	}

	exclusions, err := fdbstatus.GetExclusions(status)
	if err != nil {
		return &requeue{curError: err, delayedRequeue: true}
	}
	logger.Info("current exclusions", "exclusions", exclusions)
	fdbProcessesToExcludeByClass, ongoingExclusionsByClass := getProcessesToExclude(exclusions, cluster)

	if len(fdbProcessesToExcludeByClass) > 0 {
		var fdbProcessesToExclude []fdbv1beta2.ProcessAddress
		desiredProcesses, err := cluster.GetProcessCountsWithDefaults()
		if err != nil {
			return &requeue{curError: err, delayedRequeue: true}
		}

		desiredProcessesMap := desiredProcesses.Map()
		for processClass := range fdbProcessesToExcludeByClass {
			ongoingExclusions := ongoingExclusionsByClass[processClass]
			processesToExclude := fdbProcessesToExcludeByClass[processClass]

			allowedExclusions, missingProcesses := canExcludeNewProcesses(logger, cluster, processClass, desiredProcessesMap[processClass], ongoingExclusions, r.InSimulation)
			if allowedExclusions <= 0 {
				logger.Info("Waiting for missing processes before continuing with the exclusion", "processClass", processClass, "missingProcesses", missingProcesses, "addressesToExclude", processesToExclude, "allowedExclusions", allowedExclusions, "ongoingExclusions", ongoingExclusions)
				continue
			}

			// If we are not able to exclude all processes at once print a log message.
			if len(processesToExclude) > allowedExclusions {
				logger.Info("Some processes are still missing but continuing with the exclusion", "processClass", processClass, "missingProcesses", missingProcesses, "addressesToExclude", processesToExclude, "allowedExclusions", allowedExclusions, "ongoingExclusions", ongoingExclusions)
			}

			if len(processesToExclude) < allowedExclusions {
				allowedExclusions = len(processesToExclude)
			}

			// TODO: As a next step we could exclude transaction (log + stateless) processes together and exclude
			// storage processes with a separate call. This would make sure that no storage checks will block
			// the exclusion of transaction processes.

			// Add as many processes as allowed to the exclusion list.
			fdbProcessesToExclude = append(fdbProcessesToExclude, processesToExclude[:allowedExclusions]...)
		}

		if len(fdbProcessesToExclude) == 0 {
			return &requeue{
				message:        "more exclusions needed but not allowed, have to wait for new processes to come up",
				delayedRequeue: true,
			}
		}

		r.Recorder.Event(cluster, corev1.EventTypeNormal, "ExcludingProcesses", fmt.Sprintf("Excluding %v", fdbProcessesToExclude))

		err = adminClient.ExcludeProcesses(fdbProcessesToExclude)
		if err != nil {
			return &requeue{curError: err, delayedRequeue: true}
		}
	}

	return nil
}

func getProcessesToExclude(exclusions []fdbv1beta2.ProcessAddress, cluster *fdbv1beta2.FoundationDBCluster) (map[fdbv1beta2.ProcessClass][]fdbv1beta2.ProcessAddress, map[fdbv1beta2.ProcessClass]int) {
	fdbProcessesToExcludeByClass := make(map[fdbv1beta2.ProcessClass][]fdbv1beta2.ProcessAddress)
	// This map keeps track on how many processes are currently excluded but haven't finished the exclusion yet.
	ongoingExclusionsByClass := make(map[fdbv1beta2.ProcessClass]int)

	currentExclusionMap := make(map[string]fdbv1beta2.None, len(exclusions))
	for _, exclusion := range exclusions {
		currentExclusionMap[exclusion.String()] = fdbv1beta2.None{}
	}

	for _, processGroup := range cluster.Status.ProcessGroups {
		// Ignore process groups that are not marked for removal.
		if !processGroup.IsMarkedForRemoval() {
			continue
		}

		// Ignore all process groups that are already marked as fully excluded.
		if processGroup.IsExcluded() {
			continue
		}

		// Process already excluded using locality, so we don't have to exclude it again.
		if _, ok := currentExclusionMap[processGroup.GetExclusionString()]; ok {
			ongoingExclusionsByClass[processGroup.ProcessClass]++
			continue
		}

		// We are excluding process here using the locality field. It might be possible that the process was already excluded using IP before
		// but for the sake of consistency it is better to exclude process using locality as well.
		if cluster.UseLocalitiesForExclusion() {
			if len(fdbProcessesToExcludeByClass[processGroup.ProcessClass]) == 0 {
				fdbProcessesToExcludeByClass[processGroup.ProcessClass] = []fdbv1beta2.ProcessAddress{{StringAddress: processGroup.GetExclusionString()}}
				continue
			}

			fdbProcessesToExcludeByClass[processGroup.ProcessClass] = append(fdbProcessesToExcludeByClass[processGroup.ProcessClass], fdbv1beta2.ProcessAddress{StringAddress: processGroup.GetExclusionString()})
			continue
		}

		allAddressesExcluded := true
		for _, address := range processGroup.Addresses {
			// Already excluded, so we don't have to exclude it again.
			if _, ok := currentExclusionMap[address]; ok {
				continue
			}

			allAddressesExcluded = false
			if len(fdbProcessesToExcludeByClass[processGroup.ProcessClass]) == 0 {
				fdbProcessesToExcludeByClass[processGroup.ProcessClass] = []fdbv1beta2.ProcessAddress{{IPAddress: net.ParseIP(address)}}
				continue
			}

			fdbProcessesToExcludeByClass[processGroup.ProcessClass] = append(fdbProcessesToExcludeByClass[processGroup.ProcessClass], fdbv1beta2.ProcessAddress{IPAddress: net.ParseIP(address)})
		}

		// Only if all known addresses are excluded we assume this is an ongoing exclusion. Otherwise it might be that
		// the Pod was recreated and got a new IP address assigned.
		if allAddressesExcluded {
			ongoingExclusionsByClass[processGroup.ProcessClass]++
		}
	}

	return fdbProcessesToExcludeByClass, ongoingExclusionsByClass
}

// canExcludeNewProcesses will check if new processes for the specified process class can be excluded. The calculation takes
// the current ongoing exclusions into account and the desired process count. If there are process groups that have
// the MissingProcesses condition this method will forbid exclusions until all process groups with this condition have
// this condition for longer than ignoreMissingProcessDuration. The idea behind this is to try to exclude as many processes
// at once e.g. to reduce the number of recoveries and data movement.
func canExcludeNewProcesses(logger logr.Logger, cluster *fdbv1beta2.FoundationDBCluster, processClass fdbv1beta2.ProcessClass, desiredProcessCount int, ongoingExclusions int, inSimulation bool) (int, []fdbv1beta2.ProcessGroupID) {
	// Block excludes on missing processes not marked for removal unless they are missing for a long time and the process might be broken
	// or the namespace quota was hit.
	missingProcesses := make([]fdbv1beta2.ProcessGroupID, 0)
	validProcesses := make([]fdbv1beta2.ProcessGroupID, 0)

	exclusionsAllowed := true
	for _, processGroup := range cluster.Status.ProcessGroups {
		if processGroup.ProcessClass != processClass {
			continue
		}
		// Those should already be filtered out by the previous method.
		if processGroup.IsMarkedForRemoval() && processGroup.IsExcluded() {
			continue
		}

		missingTimestamp := processGroup.GetConditionTime(fdbv1beta2.MissingProcesses)
		if missingTimestamp != nil && !inSimulation {
			missingTime := time.Unix(*missingTimestamp, 0)
			missingProcesses = append(missingProcesses, processGroup.ProcessGroupID)
			logger.V(1).Info("Missing processes", "processGroupID", processGroup.ProcessGroupID, "missingTime", missingTime.String())

			if time.Since(missingTime) < ignoreMissingProcessDuration {
				exclusionsAllowed = false
			}
			continue
		}

		validProcesses = append(validProcesses, processGroup.ProcessGroupID)
	}

	if !exclusionsAllowed {
		logger.Info("Found at least one missing process, that was not missing for a long time", "missingProcesses", missingProcesses)
		return 0, missingProcesses
	}

	logger.V(1).Info("canExcludeNewProcesses", "validProcesses", len(validProcesses), "desiredProcessCount", desiredProcessCount, "ongoingExclusions", ongoingExclusions)

	// The assumption here is that we will only exclude a process if there is a replacement ready for it. We could relax
	// this requirement in the future and take the fault tolerance into account. We add the desired fault tolerance to
	// have some buffer to prevent cases where the operator might need to exclude more processes but there are more missing
	// processes.
	allowedExclusions := cluster.DesiredFaultTolerance() + len(validProcesses) - desiredProcessCount - ongoingExclusions

	// If automatic replacements are enabled and the allowed exclusions is less than or equal to 0, we have to check
	// how many processes are missing and if more processes are missing than the automatic replacements is allowed to
	// replace, we will allow exclusions for the count of automatic replacements removing the already ongoing exclusions.
	// This code should make sure that the operator can automatically replace processes, even in the case where multiple
	// processes are failing.
	if cluster.GetEnableAutomaticReplacements() && allowedExclusions <= 0 {
		automaticReplacements := cluster.GetMaxConcurrentAutomaticReplacements()
		if len(missingProcesses) > automaticReplacements {
			return automaticReplacements - ongoingExclusions, missingProcesses
		}
	}

	return allowedExclusions, missingProcesses
}
