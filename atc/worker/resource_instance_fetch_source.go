package worker

// this file takes in a resource and returns a source (Volume)
// we might not need to model this way

import (
	"context"
	"github.com/concourse/concourse/atc/runtime"

	"code.cloudfoundry.org/lager"
	"github.com/concourse/concourse/atc"
	"github.com/concourse/concourse/atc/db"
)

//go:generate counterfeiter . FetchSource

type FetchSource interface {
	//LockName() (string, error)
	Find() (getResultWithVolume, bool, error)
	Create(context.Context) (getResultWithVolume, error)
}

//go:generate counterfeiter . FetchSourceFactory

type FetchSourceFactory interface {
	NewFetchSource(
		logger lager.Logger,
		worker Worker,
		source atc.Source,
		params atc.Params,
		owner db.ContainerOwner,
		resourceDir string,
		cache db.UsedResourceCache,
		resourceTypes atc.VersionedResourceTypes,
		containerSpec ContainerSpec,
		processSpec ProcessSpec,
		containerMetadata db.ContainerMetadata,
		imageFetchingDelegate ImageFetchingDelegate,
	) FetchSource
}

type fetchSourceFactory struct {
	resourceCacheFactory db.ResourceCacheFactory
}

func NewFetchSourceFactory(
	resourceCacheFactory db.ResourceCacheFactory,
) FetchSourceFactory {
	return &fetchSourceFactory{
		resourceCacheFactory: resourceCacheFactory,
	}
}

func (r *fetchSourceFactory) NewFetchSource(
	logger lager.Logger,
	worker Worker,
	source atc.Source,
	params atc.Params,
	owner db.ContainerOwner,
	resourceDir string,
	cache db.UsedResourceCache,
	resourceTypes atc.VersionedResourceTypes,
	containerSpec ContainerSpec,
	processSpec ProcessSpec,
	containerMetadata db.ContainerMetadata,
	imageFetchingDelegate ImageFetchingDelegate,
) FetchSource {
	return &resourceInstanceFetchSource{
		logger:                 logger,
		worker:                 worker,
		source: source,
		params: params,
		owner: owner,
		resourceDir: resourceDir,
		cache:                  cache,
		resourceTypes:          resourceTypes,
		containerSpec:          containerSpec,
		processSpec:            processSpec,
		containerMetadata:      containerMetadata,
		imageFetchingDelegate:  imageFetchingDelegate,
		dbResourceCacheFactory: r.resourceCacheFactory,
	}
}

type resourceInstanceFetchSource struct {
	logger                 lager.Logger
	worker                 Worker
	source atc.Source
	params atc.Params
	owner db.ContainerOwner
	resourceDir string
	cache                  db.UsedResourceCache
	resourceTypes          atc.VersionedResourceTypes
	containerSpec          ContainerSpec
	processSpec            ProcessSpec
	containerMetadata      db.ContainerMetadata
	imageFetchingDelegate  ImageFetchingDelegate
	dbResourceCacheFactory db.ResourceCacheFactory
}

func findOn(logger lager.Logger, w Worker, cache db.UsedResourceCache) (volume Volume, found bool, err error) {
	return w.FindVolumeForResourceCache(
		logger,
		cache,
	)
}

func (s *resourceInstanceFetchSource) Find() (getResultWithVolume, bool, error) {
	sLog := s.logger.Session("find")
	result := getResultWithVolume{}


	volume, found, err := findOn(s.logger, s.worker, s.cache)
	if err != nil {
		sLog.Error("failed-to-find-initialized-on", err)
		return result, false, err
	}

	if !found {
		return result, false, nil
	}

	metadata, err := s.dbResourceCacheFactory.ResourceCacheMetadata(s.cache)
	if err != nil {
		sLog.Error("failed-to-get-resource-cache-metadata", err)
		return result, false, err
	}

	// TODO pass version down so it can be used in the log statement.
	//s.logger.Debug("found-initialized-versioned-source", lager.Data{"version": s.resourceInstance.Version(), "metadata": metadata.ToATCMetadata()})

	atcMetaData := []atc.MetadataField{}
	for _, m := range metadata {
		atcMetaData = append(atcMetaData, atc.MetadataField{
			Name: m.Name,
			Value: m.Value,
		})
	}


	return getResultWithVolume{
		0,
		// todo: figure out what logically should be returned for VersionResult
		runtime.VersionResult{
			Metadata: atcMetaData,
		},
		runtime.GetArtifact{VolumeHandle: volume.Handle()},
		nil,
		volume,
	},
	true, nil
}

// Create runs under the lock but we need to make sure volume does not exist
// yet before creating it under the lock
func (s *resourceInstanceFetchSource) Create(ctx context.Context) (getResultWithVolume, error) {
	sLog := s.logger.Session("create")
	result := getResultWithVolume{}
	var volume Volume

	findResult, found, err := s.Find()
	if err != nil {
		return result, err
	}

	if found {
		return findResult, nil
	}

	s.containerSpec.BindMounts = []BindMountSource{
		&CertsVolumeMount{Logger: s.logger},
	}

	container, err := s.worker.FindOrCreateContainer(
		ctx,
		s.logger,
		s.imageFetchingDelegate,
		s.owner,
		s.containerMetadata,
		s.containerSpec,
		s.resourceTypes,
	)

	if err != nil {
		sLog.Error("failed-to-construct-resource", err)
		result = getResultWithVolume{
			1,
			// todo: figure out what logically should be returned for VersionResult
			runtime.VersionResult{},
			runtime.GetArtifact{VolumeHandle: volume.Handle()},
			err,
			volume,
		}
		return result, err
	}

	mountPath := s.resourceDir
	for _, mount := range container.VolumeMounts() {
		if mount.MountPath == mountPath {
			volume = mount.Volume
			break
		}
	}

	vr := runtime.VersionResult{}
	events := make(chan runtime.Event)

	// todo: we want to decouple this resource from the container
	//res := s.resourceFactory.NewResourceForContainer(container)
	//versionedSource, err = res.Get(
	//	ctx,
	//	volume,
	//	runtime.IOConfig{
	//		Stdout: s.imageFetchingDelegate.Stdout(),
	//		Stderr: s.imageFetchingDelegate.Stderr(),
	//	},
	//	s.resourceInstance.Source(),
	//	s.resourceInstance.Params(),
	//	s.resourceInstance.Version(),
	//)
	//if err != nil {
	//	sLog.Error("failed-to-fetch-resource", err)
	//	return nil, err
	//}

	err = RunScript(
		ctx,
		container,
		s.processSpec.Path,
		s.processSpec.Args,
		runtime.GetRequest{
			Params: s.params,
			Source: s.source,
		},
		&vr,
		s.processSpec.StderrWriter,
		true,
		events,
	)

	if err != nil {
		sLog.Error("failed-to-fetch-resource", err)
		// TODO Is this compatible with previous behaviour of returning a nil when error type is NOT ErrResourceScriptFailed

		// if error returned from running the actual script
		if failErr, ok := err.(ErrResourceScriptFailed); ok {
			result = getResultWithVolume{failErr.ExitStatus, runtime.VersionResult{}, runtime.GetArtifact{}, failErr, volume}
			return result, nil
		}
		return result, err
	}

	err = volume.SetPrivileged(false)
	if err != nil {
		sLog.Error("failed-to-set-volume-unprivileged", err)
		return result, err
	}

	// TODO this should happen get_step exec rather than here
	// seems like core logic
	//err = volume.InitializeResourceCache(s.resourceInstance.ResourceCache())
	//if err != nil {
	//	sLog.Error("failed-to-initialize-cache", err)
	//	return nil, err
	//}
	//
	//err = s.dbResourceCacheFactory.UpdateResourceCacheMetadata(s.resourceInstance.ResourceCache(), versionedSource.Metadata())
	//if err != nil {
	//	s.logger.Error("failed-to-update-resource-cache-metadata", err, lager.Data{"resource-cache": s.resourceInstance.ResourceCache()})
	//	return nil, err
	//}

	return getResultWithVolume{
		VersionResult: vr,
		GetArtifact: runtime.GetArtifact{
			VolumeHandle: volume.Handle(),
		},
	}, nil
}