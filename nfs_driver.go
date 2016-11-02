package nfsdriver

import (
	"errors"
	"fmt"
	"os"

	"path/filepath"

	"syscall"

	"encoding/json"
	"sync"

	"context"
	"time"

	"code.cloudfoundry.org/goshims/execshim"
	"code.cloudfoundry.org/goshims/filepathshim"
	"code.cloudfoundry.org/goshims/ioutilshim"
	"code.cloudfoundry.org/goshims/osshim"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/voldriver"
	"code.cloudfoundry.org/voldriver/driverhttp"
)

const mountOptions = "vers=4.0,rsize=1048576,wsize=1048576,hard,intr,timeo=600,retrans=2,actimeo=0"

type NfsVolumeInfo struct {
	Ip                   string
	voldriver.VolumeInfo // see voldriver.resources.go
}

type NfsDriver struct {
	volumes       map[string]*NfsVolumeInfo
	volumesLock   sync.RWMutex
	os            osshim.Os
	filepath      filepathshim.Filepath
	exec          execshim.Exec
	ioutil        ioutilshim.Ioutil
	mountPathRoot string
	mounter       Mounter
}

func NewNfsDriver(logger lager.Logger, os osshim.Os, filepath filepathshim.Filepath, ioutil ioutilshim.Ioutil, exec execshim.Exec, mountPathRoot string, mounter Mounter) *NfsDriver {
	d := &NfsDriver{
		volumes:       map[string]*NfsVolumeInfo{},
		os:            os,
		filepath:      filepath,
		ioutil:        ioutil,
		mountPathRoot: mountPathRoot,
		mounter:       mounter,
		exec:          exec,
	}

	ctx := context.TODO()
	env := driverhttp.NewHttpDriverEnv(logger, ctx)

	d.restoreState(env)
	d.checkMounts(env)

	return d
}

func (d *NfsDriver) Activate(env voldriver.Env) voldriver.ActivateResponse {
	return voldriver.ActivateResponse{
		Implements: []string{"VolumeDriver"},
	}
}

func (d *NfsDriver) Create(env voldriver.Env, createRequest voldriver.CreateRequest) voldriver.ErrorResponse {
	logger := env.Logger().Session("create")
	logger.Info("start")
	defer logger.Info("end")

	var ok bool
	var ip string

	if createRequest.Name == "" {
		return voldriver.ErrorResponse{Err: "Missing mandatory 'volume_name'"}
	}

	if ip, ok = createRequest.Opts["ip"].(string); !ok {
		logger.Info("mount-config-missing-ip", lager.Data{"volume_name": createRequest.Name})
		return voldriver.ErrorResponse{Err: `Missing mandatory 'ip' field in 'Opts'`}
	}

	_, err := d.getVolume(driverhttp.EnvWithLogger(logger, env), createRequest.Name)

	if err != nil {
		logger.Info("creating-volume", lager.Data{"volume_name": createRequest.Name})

		volInfo := NfsVolumeInfo{
			VolumeInfo: voldriver.VolumeInfo{Name: createRequest.Name},
			Ip:         ip,
		}

		d.volumesLock.Lock()
		defer d.volumesLock.Unlock()

		d.volumes[createRequest.Name] = &volInfo

		err := d.persistState(driverhttp.EnvWithLogger(logger, env))
		if err != nil {
			logger.Error("persist-state-failed", err)
			return voldriver.ErrorResponse{Err: fmt.Sprintf("persist state failed when creating: %s", err.Error())}
		}
	}

	// TODO - if create is called twice the 'newer' create options are not retained
	return voldriver.ErrorResponse{}
}

func (d *NfsDriver) List(_ voldriver.Env) voldriver.ListResponse {
	d.volumesLock.RLock()
	defer d.volumesLock.RUnlock()

	listResponse := voldriver.ListResponse{
		Volumes: []voldriver.VolumeInfo{},
	}

	for _, volume := range d.volumes {
		listResponse.Volumes = append(listResponse.Volumes, volume.VolumeInfo)
	}
	listResponse.Err = ""
	return listResponse
}

func (d *NfsDriver) Mount(env voldriver.Env, mountRequest voldriver.MountRequest) voldriver.MountResponse {
	logger := env.Logger().Session("mount", lager.Data{"volume": mountRequest.Name})
	logger.Info("start")
	defer logger.Info("end")

	if mountRequest.Name == "" {
		return voldriver.MountResponse{Err: "Missing mandatory 'volume_name'"}
	}

	vol, err := d.getVolume(driverhttp.EnvWithLogger(logger, env), mountRequest.Name)
	if err != nil {
		return voldriver.MountResponse{Err: fmt.Sprintf("Volume '%s' must be created before being mounted", mountRequest.Name)}
	}

	mountPath := d.mountPath(driverhttp.EnvWithLogger(logger, env), vol.Name)
	logger.Info("mounting-volume", lager.Data{"id": vol.Name, "mountpoint": mountPath})

	if vol.MountCount < 1 {
		if err := d.mount(driverhttp.EnvWithLogger(logger, env), vol.Ip, mountPath); err != nil {
			logger.Error("mount-volume-failed", err)
			return voldriver.MountResponse{Err: fmt.Sprintf("Error mounting volume: %s", err.Error())}
		}
	}

	d.volumesLock.Lock()
	defer d.volumesLock.Unlock()

	// The previous vol could be stale (since it's a value copy)
	volume := d.volumes[mountRequest.Name]
	volume.Mountpoint = mountPath
	volume.MountCount++

	logger.Info("volume-mounted", lager.Data{"name": vol.Name, "count": vol.MountCount})

	if err := d.persistState(driverhttp.EnvWithLogger(logger, env)); err != nil {
		logger.Error("persist-state-failed", err)
		return voldriver.MountResponse{Err: fmt.Sprintf("persist state failed when mounting: %s", err.Error())}
	}

	mountResponse := voldriver.MountResponse{Mountpoint: volume.Mountpoint}
	return mountResponse
}

func (d *NfsDriver) Path(env voldriver.Env, pathRequest voldriver.PathRequest) voldriver.PathResponse {
	logger := env.Logger().Session("path", lager.Data{"volume": pathRequest.Name})

	if pathRequest.Name == "" {
		return voldriver.PathResponse{Err: "Missing mandatory 'volume_name'"}
	}

	vol, err := d.getVolume(driverhttp.EnvWithLogger(logger, env), pathRequest.Name)
	if err != nil {
		logger.Error("failed-no-such-volume-found", err, lager.Data{"mountpoint": vol.Mountpoint})

		return voldriver.PathResponse{Err: fmt.Sprintf("Volume '%s' not found", pathRequest.Name)}
	}

	if vol.Mountpoint == "" {
		errText := "Volume not previously mounted"
		logger.Error("failed-mountpoint-not-assigned", errors.New(errText))
		return voldriver.PathResponse{Err: errText}
	}

	return voldriver.PathResponse{Mountpoint: vol.Mountpoint}
}

func (d *NfsDriver) Unmount(env voldriver.Env, unmountRequest voldriver.UnmountRequest) voldriver.ErrorResponse {
	logger := env.Logger().Session("unmount", lager.Data{"volume": unmountRequest.Name})

	if unmountRequest.Name == "" {
		return voldriver.ErrorResponse{Err: "Missing mandatory 'volume_name'"}
	}

	vol, err := d.getVolume(driverhttp.EnvWithLogger(logger, env), unmountRequest.Name)
	if err != nil {
		logger.Error("failed-no-such-volume-found", err, lager.Data{"mountpoint": vol.Mountpoint})

		return voldriver.ErrorResponse{Err: fmt.Sprintf("Volume '%s' not found", unmountRequest.Name)}
	}

	if vol.Mountpoint == "" {
		errText := "Volume not previously mounted"
		logger.Error("failed-mountpoint-not-assigned", errors.New(errText))
		return voldriver.ErrorResponse{Err: errText}
	}

	if vol.MountCount == 1 {
		if err := d.unmount(driverhttp.EnvWithLogger(logger, env), unmountRequest.Name, vol.Mountpoint); err != nil {
			return voldriver.ErrorResponse{Err: err.Error()}
		}
	}

	d.volumesLock.Lock()
	defer d.volumesLock.Unlock()

	// The previous vol could be stale (since it's a value copy)
	if volume, ok := d.volumes[unmountRequest.Name]; ok {
		volume.MountCount--

		if volume.MountCount < 1 {
			delete(d.volumes, unmountRequest.Name)
		}

		if err = d.persistState(driverhttp.EnvWithLogger(logger, env)); err != nil {
			return voldriver.ErrorResponse{Err: fmt.Sprintf("failed to persist state when unmounting: %s", err.Error())}
		}
	}

	return voldriver.ErrorResponse{}
}

func (d *NfsDriver) Remove(env voldriver.Env, removeRequest voldriver.RemoveRequest) voldriver.ErrorResponse {
	logger := env.Logger().Session("remove", lager.Data{"volume": removeRequest})
	logger.Info("start")
	defer logger.Info("end")

	if removeRequest.Name == "" {
		return voldriver.ErrorResponse{Err: "Missing mandatory 'volume_name'"}
	}

	vol, err := d.getVolume(driverhttp.EnvWithLogger(logger, env), removeRequest.Name)

	if err != nil {
		logger.Error("warning-volume-removal", fmt.Errorf(fmt.Sprintf("Volume %s not found", removeRequest.Name)))
		return voldriver.ErrorResponse{}
	}

	if vol.Mountpoint != "" {
		if err := d.unmount(driverhttp.EnvWithLogger(logger, env), removeRequest.Name, vol.Mountpoint); err != nil {
			return voldriver.ErrorResponse{Err: err.Error()}
		}
	}

	logger.Info("removing-volume", lager.Data{"name": removeRequest.Name})

	d.volumesLock.Lock()
	defer d.volumesLock.Unlock()
	delete(d.volumes, removeRequest.Name)

	if err := d.persistState(driverhttp.EnvWithLogger(logger, env)); err != nil {
		return voldriver.ErrorResponse{Err: fmt.Sprintf("failed to persist state when removing: %s", err.Error())}
	}

	return voldriver.ErrorResponse{}
}

func (d *NfsDriver) Get(env voldriver.Env, getRequest voldriver.GetRequest) voldriver.GetResponse {
	volume, err := d.getVolume(env, getRequest.Name)
	if err != nil {
		return voldriver.GetResponse{Err: err.Error()}
	}

	return voldriver.GetResponse{
		Volume: voldriver.VolumeInfo{
			Name:       getRequest.Name,
			Mountpoint: volume.Mountpoint,
		},
	}
}

func (d *NfsDriver) getVolume(env voldriver.Env, volumeName string) (NfsVolumeInfo, error) {
	logger := env.Logger().Session("get-volume")
	d.volumesLock.RLock()
	defer d.volumesLock.RUnlock()

	if vol, ok := d.volumes[volumeName]; ok {
		logger.Info("getting-volume", lager.Data{"name": volumeName})
		return *vol, nil
	}

	return NfsVolumeInfo{}, errors.New("Volume not found")
}

func (d *NfsDriver) Capabilities(env voldriver.Env) voldriver.CapabilitiesResponse {
	return voldriver.CapabilitiesResponse{
		Capabilities: voldriver.CapabilityInfo{Scope: "local"},
	}
}

func (d *NfsDriver) exists(path string) (bool, error) {
	_, err := d.os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

func (d *NfsDriver) mountPath(env voldriver.Env, volumeId string) string {
	logger := env.Logger().Session("mount-path")
	orig := syscall.Umask(000)
	defer syscall.Umask(orig)

	dir, err := d.filepath.Abs(d.mountPathRoot)
	if err != nil {
		logger.Fatal("abs-failed", err)
	}

	if err := d.os.MkdirAll(dir, os.ModePerm); err != nil {
		logger.Fatal("mkdir-rootpath-failed", err)
	}

	return filepath.Join(dir, volumeId)
}

func (d *NfsDriver) mount(env voldriver.Env, ip, mountPath string) error {
	logger := env.Logger().Session("mount", lager.Data{"ip": ip, "target": mountPath})
	logger.Info("start")
	defer logger.Info("end")

	orig := syscall.Umask(000)
	defer syscall.Umask(orig)

	err := d.os.MkdirAll(mountPath, os.ModePerm)
	if err != nil {
		logger.Error("create-mountdir-failed", err)
		return err
	}

	// TODO--permissions & flags?
	output, err := d.mounter.Mount(ip+":/", mountPath, "nfs4", 0, mountOptions)
	if err != nil {
		logger.Error("mount-failed: "+string(output), err)
		err = fmt.Errorf("%s:(%s)", output, err.Error())
	}
	return err
}

func (d *NfsDriver) persistState(env voldriver.Env) error {
	// TODO--why are we passing state instead of using the one in d?

	logger := env.Logger().Session("persist-state")
	logger.Info("start")
	defer logger.Info("end")

	orig := syscall.Umask(000)
	defer syscall.Umask(orig)

	stateFile := filepath.Join(d.mountPathRoot, "efs-broker-state.json")

	stateData, err := json.Marshal(d.volumes)
	if err != nil {
		logger.Error("failed-to-marshall-state", err)
		return err
	}

	err = d.ioutil.WriteFile(stateFile, stateData, os.ModePerm)
	if err != nil {
		logger.Error(fmt.Sprintf("failed-to-write-state-file: %s", stateFile), err)
		return err
	}

	logger.Debug("state-saved", lager.Data{"state-file": stateFile})
	return nil
}

func (d *NfsDriver) restoreState(env voldriver.Env) {
	logger := env.Logger().Session("restore-state")
	logger.Info("start")
	defer logger.Info("end")

	stateFile := filepath.Join(d.mountPathRoot, "efs-broker-state.json")

	stateData, err := d.ioutil.ReadFile(stateFile)
	if err != nil {
		logger.Info(fmt.Sprintf("failed-to-read-state-file: %s", stateFile), lager.Data{"err": err})
		return
	}

	state := map[string]*NfsVolumeInfo{}
	err = json.Unmarshal(stateData, &state)

	if err != nil {
		logger.Error(fmt.Sprintf("failed-to-unmarshall-state from state-file: %s", stateFile), err)
		return
	}
	logger.Info("state-restored", lager.Data{"state-file": stateFile})

	d.volumesLock.Lock()
	defer d.volumesLock.Unlock()
	d.volumes = state
}

func (d *NfsDriver) unmount(env voldriver.Env, name string, mountPath string) error {
	logger := env.Logger().Session("unmount")
	logger.Info("start")
	defer logger.Info("end")

	exists, err := d.exists(mountPath)
	if err != nil {
		logger.Error("failed-retrieving-mount-info", err, lager.Data{"mountpoint": mountPath})
		return errors.New("Error establishing whether volume exists")
	}

	if !exists {
		errText := fmt.Sprintf("Volume %s does not exist (path: %s), nothing to do!", name, mountPath)
		logger.Error("failed-mountpoint-not-found", errors.New(errText))
		return errors.New(errText)
	}

	logger.Info("unmount-volume-folder", lager.Data{"mountpath": mountPath})

	err = d.mounter.Unmount(mountPath, 0)
	if err != nil {
		logger.Error("unmount-failed", err)
		return fmt.Errorf("Error unmounting volume: %s", err.Error())
	}
	err = d.os.RemoveAll(mountPath)
	if err != nil {
		logger.Error("create-mountdir-failed", err)
		return fmt.Errorf("Error creating mountpoint: %s", err.Error())
	}

	logger.Info("unmounted-volume")

	return nil
}

func (d *NfsDriver) checkMounts(env voldriver.Env) {
	logger := env.Logger().Session("check-mounts")
	logger.Info("start")
	defer logger.Info("end")

	for key, mount := range d.volumes {
		ctx, _ := context.WithDeadline(context.TODO(), time.Now().Add(time.Second*5))
		cmd := d.exec.CommandContext(ctx, "mountpoint", "-q", mount.VolumeInfo.Mountpoint)

		if err := cmd.Start(); err != nil {
			logger.Error("unexpected-command-invocation-error", err)
			continue
		}

		if err := cmd.Wait(); err != nil {
			// Note: Created volumes (with no mounts) will be removed
			//       since VolumeInfo.Mountpoint will be an empty string
			logger.Info(fmt.Sprintf("unable to verify volume %s (%s)", key, err.Error()))
			delete(d.volumes, key)
		}
	}

}
