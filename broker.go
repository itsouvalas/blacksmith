package main

import (
	"fmt"
	"log"
	"os"

	"github.com/cloudfoundry-community/gogobosh"
	"github.com/pivotal-cf/brokerapi"
	"github.com/pivotal-golang/lager"
)

type Broker struct {
	Catalog []brokerapi.Service
	Plans   map[string]Plan
	BOSH    *gogobosh.Client
	Vault   *Vault
	logger  lager.Logger
}

var Debugging bool

func init() {
	Debugging = os.Getenv("BLACKSMITH_DEBUG") != ""
}

func Debugf(fmt string, args ...interface{}) {
	if Debugging {
		log.Printf("DEBUG: %s", args...)
	}
}

func (b Broker) FindPlan(planID string, serviceID string) (Plan, error) {
	Debugf("FindPlan: looking for plan '%s' and service '%s'", planID, serviceID)

	key := fmt.Sprintf("%s/%s", planID, serviceID)
	if plan, ok := b.Plans[key]; ok {
		return plan, nil
	}
	return Plan{}, fmt.Errorf("plan %s not found", key)
}

func (b *Broker) Services() []brokerapi.Service {
	log.Printf("[catalog] returning service catalog")
	return b.Catalog
}

func (b *Broker) ReadServices(dir ...string) error {
	ss, err := ReadServices(dir...)
	if err != nil {
		return err
	}

	b.Catalog = Catalog(ss)
	b.Plans = make(map[string]Plan)
	for _, s := range ss {
		for _, p := range s.Plans {
			Debugf("tracking service/plan %s/%s", s.ID, p.ID)
			b.Plans[fmt.Sprintf("%s/%s", s.ID, p.ID)] = p
		}
	}

	return nil
}

func (b *Broker) Provision(instanceID string, details brokerapi.ProvisionDetails, asyncAllowed bool) (brokerapi.ProvisionedServiceSpec, error) {
	spec := brokerapi.ProvisionedServiceSpec{IsAsync: true}
	log.Printf("[provision %s] provisioning new service", instanceID)

	plan, err := b.FindPlan(details.ServiceID, details.PlanID)
	if err != nil {
		log.Printf("[provision %s] failed: %s", instanceID, err)
		return spec, err
	}

	params := make(map[interface{}]interface{})
	params["name"] = plan.Name + "-" + instanceID

	info, err := b.BOSH.GetInfo()
	if err != nil {
		log.Printf("[provision %s] failed to get info from BOSH: %s", instanceID, err)
		return spec, fmt.Errorf("BOSH deployment manifest generation failed")
	}
	params["director_uuid"] = info.UUID

	manifest, creds, err := GenManifest(plan, params)
	if err != nil {
		log.Printf("[provision %s] failed to generate manifest: %s", instanceID, err)
		return spec, fmt.Errorf("BOSH deployment manifest generation failed")
	}

	Debugf("generated manifest:\n%s", manifest)
	task, err := b.BOSH.CreateDeployment(manifest)
	if err != nil {
		log.Printf("[provision %s] failed to create deployment: %s", instanceID, err)
		return spec, fmt.Errorf("backend BOSH deployment failed")
	}

	err = b.Vault.Track(instanceID, "provision", task.ID, creds)
	if err != nil {
		log.Printf("[provision %s] failed to track deployment: %s", instanceID, err)
		return spec, fmt.Errorf("Vault tracking failed")
	}
	log.Printf("[provision %s] started", instanceID)
	return spec, nil
}

func (b *Broker) Deprovision(instanceID string, details brokerapi.DeprovisionDetails, asyncAllowed bool) (brokerapi.IsAsync, error) {
	log.Printf("[deprovision %s] deleting deployment %s", instanceID, instanceID)
	/* FIXME: what if we still have a valid task for deployment? */
	task, err := b.BOSH.DeleteDeployment(instanceID)
	if err != nil {
		return true, err
	}

	b.Vault.Track(instanceID, "deprovision", task.ID, nil)
	log.Printf("[deprovision %s] started", instanceID)
	return true, nil
}

func (b *Broker) LastOperation(instanceID string) (brokerapi.LastOperation, error) {
	typ, taskID, _, _ := b.Vault.State(instanceID)
	if typ == "provision" {
		task, err := b.BOSH.GetTask(taskID)
		if err != nil {
			log.Printf("[provision %s] failed to get task from BOSH: %s", instanceID, err)
			return brokerapi.LastOperation{}, fmt.Errorf("unrecognized backend BOSH task")
		}

		if task.State == "done" {
			log.Printf("[provision %s] succeeded", instanceID)
			return brokerapi.LastOperation{State: "succeeded"}, nil
		}
		if task.State == "error" {
			log.Printf("[provision %s] failed", instanceID)
			return brokerapi.LastOperation{State: "failed"}, nil
		}

		return brokerapi.LastOperation{State: "in progress"}, nil
	}

	if typ == "deprovision" {
		task, err := b.BOSH.GetTask(taskID)
		if err != nil {
			log.Printf("[deprovision %s] failed to get task from BOSH: %s", instanceID, err)
			return brokerapi.LastOperation{}, fmt.Errorf("unrecognized backend BOSH task")
		}

		if task.State == "done" {
			log.Printf("[deprovision %s] task completed", instanceID)
			log.Printf("[deprovision %s] clearing secrets under secret/%s", instanceID, instanceID)
			b.Vault.Clear(instanceID)

			log.Printf("[deprovision %s] succeeded", instanceID)
			return brokerapi.LastOperation{State: "succeeded"}, nil
		}

		if task.State == "error" {
			log.Printf("[deprovision %s] clearing secrets under secret/%s", instanceID, instanceID)
			b.Vault.Clear(instanceID)

			log.Printf("[deprovision %s] failed", instanceID)
			return brokerapi.LastOperation{State: "failed"}, nil
		}

		return brokerapi.LastOperation{State: "in progress"}, nil
	}

	return brokerapi.LastOperation{}, fmt.Errorf("invalid state type '%s'", typ)
}

func (b *Broker) Bind(instanceID, bindingID string, details brokerapi.BindDetails) (brokerapi.Binding, error) {
	var binding brokerapi.Binding
	log.Printf("[bind %s / %s] binding service", instanceID, bindingID)

	_, _, creds, _ := b.Vault.State(instanceID)
	binding.Credentials = creds

	log.Printf("[bind %s / %s] success", instanceID, bindingID)
	return binding, nil
}

func (b *Broker) Unbind(instanceID, bindingID string, details brokerapi.UnbindDetails) error {
	return nil
}

func (b *Broker) Update(instanceID string, details brokerapi.UpdateDetails, asyncAllowed bool) (brokerapi.IsAsync, error) {
	// Update instance here
	return false, fmt.Errorf("not implemented")
}
