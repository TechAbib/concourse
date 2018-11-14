package exec

import (
	"context"

	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagerctx"
	"github.com/concourse/concourse/atc"
	"github.com/concourse/concourse/atc/creds"
	"github.com/concourse/concourse/atc/db"
	"github.com/concourse/concourse/atc/resource"
	"github.com/concourse/concourse/atc/worker"
)

//go:generate counterfeiter . PutDelegate

type PutDelegate interface {
	BuildStepDelegate

	Finished(lager.Logger, ExitStatus, VersionInfo)
}

// PutStep produces a resource version using preconfigured params and any data
// available in the worker.ArtifactRepository.
type PutStep struct {
	build db.Build

	name         string
	resourceType string
	resource     string
	source       creds.Source
	params       creds.Params
	tags         atc.Tags
	inputs       PutInputs

	delegate              PutDelegate
	resourceFactory       resource.ResourceFactory
	resourceConfigFactory db.ResourceConfigFactory
	planID                atc.PlanID
	containerMetadata     db.ContainerMetadata
	stepMetadata          StepMetadata

	resourceTypes creds.VersionedResourceTypes

	versionInfo VersionInfo
	succeeded   bool
}

func NewPutStep(
	build db.Build,
	name string,
	resourceType string,
	resourceName string,
	source creds.Source,
	params creds.Params,
	tags atc.Tags,
	inputs PutInputs,
	delegate PutDelegate,
	resourceFactory resource.ResourceFactory,
	resourceConfigFactory db.ResourceConfigFactory,
	planID atc.PlanID,
	containerMetadata db.ContainerMetadata,
	stepMetadata StepMetadata,
	resourceTypes creds.VersionedResourceTypes,
) *PutStep {
	return &PutStep{
		build: build,

		resourceType:          resourceType,
		name:                  name,
		resource:              resourceName,
		source:                source,
		params:                params,
		tags:                  tags,
		inputs:                inputs,
		delegate:              delegate,
		resourceFactory:       resourceFactory,
		resourceConfigFactory: resourceConfigFactory,
		planID:                planID,
		containerMetadata:     containerMetadata,
		stepMetadata:          stepMetadata,
		resourceTypes:         resourceTypes,
	}
}

// Run chooses a worker that supports the step's resource type and creates a
// container.
//
// All worker.ArtifactSources present in the worker.ArtifactRepository are then brought into
// the container, using volumes if possible, and streaming content over if not.
//
// The resource's put script is then invoked. If the context is canceled, the
// script will be interrupted.
func (step *PutStep) Run(ctx context.Context, state RunState) error {
	logger := lagerctx.FromContext(ctx)

	containerInputs, err := step.inputs.FindAll(state.Artifacts())
	if err != nil {
		return err
	}

	containerSpec := worker.ContainerSpec{
		ImageSpec: worker.ImageSpec{
			ResourceType: step.resourceType,
		},
		Tags:   step.tags,
		TeamID: step.build.TeamID(),

		Dir: resource.ResourcesDir("put"),

		Env: step.stepMetadata.Env(),

		Inputs: containerInputs,
	}

	putResource, err := step.resourceFactory.NewResource(
		ctx,
		logger,
		db.NewBuildStepContainerOwner(step.build.ID(), step.planID, step.build.TeamID()),
		step.containerMetadata,
		containerSpec,
		step.resourceTypes,
		step.delegate,
	)
	if err != nil {
		return err
	}

	source, err := step.source.Evaluate()
	if err != nil {
		return err
	}

	params, err := step.params.Evaluate()
	if err != nil {
		return err
	}

	versionedSource, err := putResource.Put(
		ctx,
		resource.IOConfig{
			Stdout: step.delegate.Stdout(),
			Stderr: step.delegate.Stderr(),
		},
		source,
		params,
	)

	if err != nil {
		logger.Error("failed-to-put-resource", err)

		if err, ok := err.(resource.ErrResourceScriptFailed); ok {
			step.delegate.Finished(logger, ExitStatus(err.ExitStatus), VersionInfo{})
			return nil
		}

		return err
	}

	step.versionInfo = VersionInfo{
		Version:  versionedSource.Version(),
		Metadata: versionedSource.Metadata(),
	}

	if step.resource != "" {
		logger = logger.WithData(lager.Data{"step": step.name, "resource": step.resource, "resource-type": step.resourceType, "version": step.versionInfo.Version})
		resourceConfig, err := step.resourceConfigFactory.FindOrCreateResourceConfig(logger, step.resourceType, source, step.resourceTypes)
		if err != nil {
			logger.Error("failed-to-find-or-create-resource-config", err)
			return err
		}

		created, err := resourceConfig.SaveUncheckedVersion(step.versionInfo.Version, db.NewResourceConfigMetadataFields(step.versionInfo.Metadata))
		if err != nil {
			logger.Error("failed-to-save-version", err)
			return err
		}

		err = step.build.SaveOutput(resourceConfig, step.versionInfo.Version, step.name, step.resource, created)
		if err != nil {
			logger.Error("failed-to-save-output", err)
			return err
		}
	}

	state.StoreResult(step.planID, step.versionInfo)

	step.succeeded = true

	step.delegate.Finished(logger, 0, step.versionInfo)

	return nil
}

// VersionInfo returns the info of the pushed version.
func (step *PutStep) VersionInfo() VersionInfo {
	return step.versionInfo
}

// Succeeded returns true if the resource script exited successfully.
func (step *PutStep) Succeeded() bool {
	return step.succeeded
}
