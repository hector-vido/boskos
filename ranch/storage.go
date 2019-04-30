/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ranch

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/sirupsen/logrus"
	"sigs.k8s.io/yaml"

	"k8s.io/test-infra/boskos/common"
	"k8s.io/test-infra/boskos/storage"
)

// Storage is used to decouple ranch functionality with the resource persistence layer
type Storage struct {
	resources, dynamicResourceLifeCycles storage.PersistenceLayer
	resourcesLock                        sync.RWMutex

	// For testing
	UpdateTime func() time.Time
}

// NewStorage instantiates a new Storage with a PersistenceLayer implementation
// If storage string is not empty, it will read resource data from the file
func NewStorage(r storage.PersistenceLayer, storage string) (*Storage, error) {
	s := &Storage{
		resources: r,
	}

	if storage != "" {
		var data struct {
			Resources []common.Resource
		}
		buf, err := ioutil.ReadFile(storage)
		if err == nil {
			logrus.Infof("Current state: %s.", string(buf))
			err = json.Unmarshal(buf, &data)
			if err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}

		for _, res := range data.Resources {
			if err := s.AddResource(res); err != nil {
				logrus.WithError(err).Errorf("Failed Adding Resource: %s - %s.", res.Name, res.State)
			}
			logrus.Infof("Successfully Added Resource: %s - %s.", res.Name, res.State)
		}
	}
	return s, nil
}

// AddResource adds a new resource
func (s *Storage) AddResource(resource common.Resource) error {
	return s.resources.Add(resource)
}

// DeleteResource deletes a resource if it exists, errors otherwise
func (s *Storage) DeleteResource(name string) error {
	return s.resources.Delete(name)
}

// UpdateResource updates a resource if it exists, errors otherwise
func (s *Storage) UpdateResource(resource common.Resource) error {
	resource.LastUpdate = s.UpdateTime()
	return s.resources.Update(resource)
}

// GetResource gets an existing resource, errors otherwise
func (s *Storage) GetResource(name string) (common.Resource, error) {
	i, err := s.resources.Get(name)
	if err != nil {
		return common.Resource{}, err
	}
	var res common.Resource
	res, err = common.ItemToResource(i)
	if err != nil {
		return common.Resource{}, err
	}
	return res, nil
}

// GetResources list all resources
func (s *Storage) GetResources() ([]common.Resource, error) {
	var resources []common.Resource
	items, err := s.resources.List()
	if err != nil {
		return resources, err
	}
	for _, i := range items {
		var res common.Resource
		res, err = common.ItemToResource(i)
		if err != nil {
			return nil, err
		}
		resources = append(resources, res)
	}
	sort.Stable(common.ResourceByUpdateTime(resources))
	return resources, nil
}

// AddDynamicResourceLifeCycle adds a new resource
func (s *Storage) AddDynamicResourceLifeCycle(resource common.DynamicResourceLifeCycle) error {
	return s.dynamicResourceLifeCycles.Add(resource)
}

// DeleteDynamicResourceLifeCycle deletes a resource if it exists, errors otherwise
func (s *Storage) DeleteDynamicResourceLifeCycle(name string) error {
	return s.dynamicResourceLifeCycles.Delete(name)
}

// UpdateDynamicResourceLifeCycle updates a resource if it exists, errors otherwise
func (s *Storage) UpdateDynamicResourceLifeCycle(resource common.DynamicResourceLifeCycle) error {
	return s.dynamicResourceLifeCycles.Update(resource)
}

// GetDynamicResourceLifeCycle gets an existing resource, errors otherwise
func (s *Storage) GetDynamicResourceLifeCycle(name string) (common.DynamicResourceLifeCycle, error) {
	i, err := s.dynamicResourceLifeCycles.Get(name)
	if err != nil {
		return common.DynamicResourceLifeCycle{}, err
	}
	var res common.DynamicResourceLifeCycle
	res, err = common.ItemToDynamicResourceLifeCycle(i)
	if err != nil {
		return common.DynamicResourceLifeCycle{}, err
	}
	return res, nil
}

// GetResourcesLifeCycle list all resources
func (s *Storage) GetDynamicResourceLifeCycles() ([]common.DynamicResourceLifeCycle, error) {
	var resources []common.DynamicResourceLifeCycle
	items, err := s.dynamicResourceLifeCycles.List()
	if err != nil {
		return resources, err
	}
	for _, i := range items {
		var res common.DynamicResourceLifeCycle
		res, err = common.ItemToDynamicResourceLifeCycle(i)
		if err != nil {
			return nil, err
		}
		resources = append(resources, res)
	}
	return resources, nil
}

// SyncResources will update static and dynamic resources periodically.
// It will append newly added resources to storage,
// And try to remove newly deleted resources from storage.
// If the newly deleted resource is currently held by a user, the deletion will
// yield to next update cycle.
func (s *Storage) SyncResources(config *common.BoskosConfig) error {
	var newSR, existingSR, existingDR []common.Resource
	var newDRLC []common.DynamicResourceLifeCycle

	s.resourcesLock.Lock()
	defer s.resourcesLock.Unlock()

	resources, err := s.GetResources()
	if err != nil {
		logrus.WithError(err).Error("cannot find resources")
		return err
	}
	existingDRLC, err := s.GetDynamicResourceLifeCycles()
	if err != nil {
		logrus.WithError(err).Error("cannot find dynamicResourceLifeCycles")
		return err
	}

	// Split resources between static and dynamic resources
	var lifeCycleTypes map[string]bool
	for _, lc := range existingDRLC {
		lifeCycleTypes[lc.Type] = true
	}
	for _, res := range resources {
		if lifeCycleTypes[res.Type] {
			existingDR = append(existingDR, res)
		} else {
			existingSR = append(existingSR, res)
		}
	}

	for _, entry := range config.Resources {
		if len(entry.Names) > 0 {
			newSR = append(newSR, common.NewResourcesFromConfig(entry)...)
		} else {
			newDRLC = append(newDRLC, common.NewDynamicResourceLifeCycleFromConfig(entry))
		}

	}

	if err := s.syncStaticResources(newSR, existingSR); err != nil {
		return err
	}
	if err := s.syncDynamicResources(newDRLC, existingDRLC, existingDR); err != nil {
		return err
	}
	return nil
}

// generateDynamicResourceName generate a new dynamic resource name based on existing resource names.
func generateDynamicResourceName(rType string, existingResources []common.Resource) string {
	i := 0
	var name string
	exists := map[string]bool{}

	for _, r := range existingResources {
		exists[r.Name] = true
	}

	for {
		name = fmt.Sprintf("%s_%d", rType, i)
		if !exists[name] {
			return name
		} else {
			i++
		}
	}
}

// updateDynamicResources will update dynamic resource based on existing on a ResourceTypePolicy.
// It will check for all the resources and delete all the resources that are expired.
func (s *Storage) updateDynamicResources(lifecycle common.DynamicResourceLifeCycle, resources []common.Resource) (toUpdate, toAdd, toDelete []common.Resource) {
	var notInUseRes []common.Resource
	count := 0
	for _, r := range resources {
		// We can only delete project not in use
		if !r.IsInUsed() {
			if r.IsExpired() {
				toDelete = append(toDelete, r)
			} else {
				notInUseRes = append(notInUseRes, r)
				count++
			}
		} else {
			count++
		}
	}

	for i := count; i < lifecycle.MinCount; i++ {
		name := generateDynamicResourceName(lifecycle.Type, resources)
		res := common.NewResource(name, lifecycle.Type, lifecycle.InitialState, "", s.UpdateTime())
		toAdd = append(toAdd, res)
		count++
	}
	numberOfResToDelete := count - lifecycle.MaxCount
	for i := 0; i < len(notInUseRes); i++ {
		res := notInUseRes[i]
		if i < numberOfResToDelete {
			toDelete = append(toDelete, res)
		} else {
			toUpdate = append(toUpdate, res)
		}
	}
	return
}

func (s *Storage) syncDynamicResources(newConfig, existingConfig []common.DynamicResourceLifeCycle, existingResources []common.Resource) error {
	var finalError error
	var resToUpdate, resToAdd, resToDelete []common.Resource
	var dRLCToUpdate, dRLCToAdd, dRLCToDelete []common.DynamicResourceLifeCycle

	newLFSet := map[string]common.DynamicResourceLifeCycle{}
	for _, lf := range newConfig {
		newLFSet[lf.Type] = lf
	}

	// classifying existing resources by type
	existingResByType := map[string][]common.Resource{}
	for _, r := range existingResources {
		existingResByType[r.Type] = append(existingResByType[r.Type], r)
	}

	for _, existingLF := range existingConfig {
		newFL, exists := newLFSet[existingLF.Type]
		if exists {
			dRLCToUpdate = append(dRLCToUpdate, newFL)
			moreToUpdate, moreToAdd, moreToDelete := s.updateDynamicResources(newFL, existingResByType[newFL.Type])
			resToUpdate = append(resToUpdate, moreToUpdate...)
			resToAdd = append(resToAdd, moreToAdd...)
			resToAdd = append(resToDelete, moreToDelete...)
		} else {
			dRLCToDelete = append(dRLCToDelete, existingLF)
			for _, res := range existingResByType[existingLF.Type] {
				resToDelete = append(resToDelete, res)
			}
		}
	}

	for _, newDRLC := range newConfig {
		_, exists := existingResByType[newDRLC.Type]
		if !exists {
			dRLCToAdd = append(dRLCToAdd, newDRLC)
		}
	}

	if err := s.persistResources(resToUpdate, resToAdd, resToDelete); err != nil {
		finalError = multierror.Append(finalError, err)
	}
	if err := s.persistDynamicResourceLifeCycles(dRLCToUpdate, dRLCToAdd, dRLCToDelete); err != nil {
		finalError = multierror.Append(finalError, err)
	}
	return finalError
}

func (s *Storage) persistResources(resToUpdate, resToAdd, resToDelete []common.Resource) error {
	var finalError error

	for _, r := range resToDelete {
		// If currently busy, yield deletion to later cycles.
		if !r.IsInUsed() {
			logrus.Infof("Deleting resource %s", r.Name)
			if err := s.DeleteResource(r.Name); err != nil {
				finalError = multierror.Append(finalError, err)
				logrus.WithError(err).Errorf("unable to delete resource %s", r.Name)
			}
		}
	}

	for _, r := range resToAdd {
		logrus.Infof("Adding resource %s", r.Name)
		if err := s.AddResource(r); err != nil {
			finalError = multierror.Append(finalError, err)
			logrus.WithError(err).Errorf("unable to delete resource %s", r.Name)
		}
	}

	for _, r := range resToUpdate {
		logrus.Infof("Updating resource %s", r.Name)
		if err := s.UpdateResource(r); err != nil {
			finalError = multierror.Append(finalError, err)
			logrus.WithError(err).Errorf("unable to updateresource %s", r.Name)
		}
	}
	return finalError
}

func (s *Storage) persistDynamicResourceLifeCycles(dRLCToUpdate, dRLCToAdd, dRLCToDelelete []common.DynamicResourceLifeCycle) error {
	var finalError error

	for _, dRLC := range dRLCToUpdate {
		logrus.Infof("Updating resource type life cycle %s", dRLC.Type)
		if err := s.UpdateDynamicResourceLifeCycle(dRLC); err != nil {
			finalError = multierror.Append(finalError, err)
			logrus.WithError(err).Errorf("unable to update resource type life cycle %s", dRLC.Type)
		}
	}

	for _, DRLC := range dRLCToAdd {
		logrus.Infof("Adding resource type life cycle %s", DRLC.Type)
		if err := s.AddDynamicResourceLifeCycle(DRLC); err != nil {
			finalError = multierror.Append(finalError, err)
			logrus.WithError(err).Errorf("unable to add resource type life cycle %s", DRLC.Type)
		}
	}

	for _, dRLC := range dRLCToDelelete {
		logrus.Infof("Deleting resource type life cycle %s", dRLC.Type)
		if err := s.DeleteDynamicResourceLifeCycle(dRLC.Type); err != nil {
			finalError = multierror.Append(finalError, err)
			logrus.WithError(err).Errorf("unable to delete resource type life cycle %s", dRLC.Type)
		}
	}

	return finalError
}

func (s *Storage) syncStaticResources(newConfig, existingResources []common.Resource) error {
	var resToUpdate, resToAdd, resToDelete []common.Resource

	// delete non-exist resource
	newResSet := map[string]bool{}
	remainingSet := map[string]bool{}

	for _, r := range newConfig {
		newResSet[r.Name] = true
	}

	for _, res := range existingResources {
		if newResSet[res.Name] {
			remainingSet[res.Name] = true
			// This is a resource owned by Boskos, Need to update it.
			resToUpdate = append(resToUpdate, res)

		} else {
			resToDelete = append(resToDelete, res)
		}
	}

	// add new resource
	for _, p := range newConfig {
		if !remainingSet[p.Name] {
			resToAdd = append(resToAdd, p)
		}
	}
	return s.persistResources(resToUpdate, resToAdd, resToDelete)
}

// ParseConfig reads in configPath and returns a list of resource objects
// on success.
func ParseConfig(configPath string) (*common.BoskosConfig, error) {
	file, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var data common.BoskosConfig
	err = yaml.Unmarshal(file, &data)
	if err != nil {
		return nil, err
	}
	return &data, nil
}
