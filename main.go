package main

import (
    "fmt"
    "log"
    "os"
    "os/exec"
    "path"
    "strconv"
    "strings"
    "sync"
    "syscall"
    "github.com/docker/go-plugins-helpers/volume"
)

type glusterDriver struct {
    disk    string
    hosts   []string
    mutex   *sync.RWMutex
    scope   string
    socket  string
    volume.Driver
}

func (driver *glusterDriver) Capabilities() *volume.CapabilitiesResponse {
    return &volume.CapabilitiesResponse{Capabilities: volume.Capability{Scope: driver.scope}}
}

func (driver *glusterDriver) Create(req *volume.CreateRequest) error {
    driver.mutex.Lock()
    defer driver.mutex.Unlock()

    if exists, err := driver.VolumeExists(req.Name); err != nil {
        return err
    } else if exists {
        return fmt.Errorf("volume %s already exists", req.Name)
    }

    var volumeHosts []string
    if hosts, exists := req.Options["hosts"]; exists {
        volumeHosts = strings.Split(hosts, ",")
    } else {
        // default to global hosts if not specified
        volumeHosts = driver.hosts
    }

    var volumeDisk string
    if disk, exists := req.Options["disk"]; exists {
        volumeDisk = disk
    } else {
        // default to default disk
        volumeDisk = driver.disk
    }

    volumeCreateCommand := []string {"gluster", "volume", "create", req.Name}
    volumeCreateCommand = append(volumeCreateCommand, "replica", strconv.Itoa(len(volumeHosts)))
    for _, host := range volumeHosts {
        volumeCreateCommand = append(volumeCreateCommand, host+":"+path.Join(volumeDisk, req.Name))
    }

    if out, err := exec.Command(volumeCreateCommand[0], volumeCreateCommand[1:]...).CombinedOutput(); err != nil {
        return fmt.Errorf(
            "error creating gluster volume: %s [%s]",
            string(out),
            strings.Join(volumeCreateCommand, " "))
    }

    // start gluster vol
    volumeStartCommand := []string {"gluster", "volume", "start", req.Name}
    if out, err := exec.Command(volumeStartCommand[0], volumeStartCommand[1:]...).CombinedOutput(); err != nil {
        return fmt.Errorf(
            "error starting gluster volume: %s [%s]",
            string(out),
            strings.Join(volumeStartCommand, " "))
    }

    // gluster volume quota docker1 enable
    // gluster volume quota docker1 limit-usage / 5GB
    // gluster volume set docker1 quota-deem-statfs on
    if volumeSize, volumeHasQuota := req.Options["size"]; volumeHasQuota {
        volumeQuotaCommands := [][]string {
            {"gluster", "volume", "quota", req.Name, "enable"},
            {"gluster", "volume", "quota", req.Name, "limit-usage", "/", volumeSize},
            {"gluster", "volume", "set", req.Name, "quota-deem-statfs", "on"},
        }
        for _, cmd := range volumeQuotaCommands {
            if out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
                exec.Command("gluster", "volume", "stop", req.Name, "--mode=script").Run()
                return fmt.Errorf(
                    "error setting gluster volume quota: %s [%s]",
                    string(out),
                    strings.Join(cmd, " "))
            }
        }
    }

    return nil
}

func (driver *glusterDriver) Get(req *volume.GetRequest) (*volume.GetResponse, error) {
    driver.mutex.RLock()
    defer driver.mutex.RUnlock()

    if exists, err := driver.VolumeExists(req.Name); err != nil {
        return &volume.GetResponse{}, err
    } else if exists {
        return &volume.GetResponse{
            Volume: &volume.Volume{
                Name: req.Name,
            },
        }, nil
    }

    return &volume.GetResponse{}, fmt.Errorf("volume %s does not exist", req.Name)
}

func (driver *glusterDriver) List() (*volume.ListResponse, error) {
    driver.mutex.RLock()
    defer driver.mutex.RUnlock()

    var volumes []*volume.Volume

    glusterVolumes, err := driver.ListVolumes()
    if err != nil {
        return &volume.ListResponse{}, err
    }

    for _, elem := range glusterVolumes {
        volumes = append(volumes, &volume.Volume{
            Name: elem,
        })
    }
    return &volume.ListResponse{Volumes: volumes}, nil
}

func (driver *glusterDriver) Mount(req *volume.MountRequest) (*volume.MountResponse, error) {
    driver.mutex.Lock()
    defer driver.mutex.Unlock()

    if exists, err := driver.VolumeExists(req.Name); err != nil {
        return &volume.MountResponse{}, err
    } else if !exists {
        return &volume.MountResponse{}, fmt.Errorf("volume %s does not exist", req.Name)
    }

    mountPoint := path.Join(volume.DefaultDockerRootDirectory, req.ID, req.Name)
    if err := os.MkdirAll(mountPoint, 0755); err != nil {
        return &volume.MountResponse{}, fmt.Errorf("error creating mountpoint %s: %s", mountPoint, err.Error())
    }

    if out, err := exec.Command(
        "mount", "-t", "glusterfs", "localhost:/" + req.Name,
        mountPoint).CombinedOutput(); err != nil {
        return &volume.MountResponse{}, fmt.Errorf("error mounting gluster volume: %s", string(out))
    }

    return &volume.MountResponse{
        Mountpoint: mountPoint,
    }, nil
}

func (driver *glusterDriver) Unmount(req *volume.UnmountRequest) error {
    driver.mutex.Lock()
    defer driver.mutex.Unlock()

    if exists, err := driver.VolumeExists(req.Name); err != nil {
        return err
    } else if !exists {
        return fmt.Errorf("volume %s does not exist", req.Name)
    }

    mountPoint := path.Join(volume.DefaultDockerRootDirectory, req.ID, req.Name)
    if err := syscall.Unmount(mountPoint, 0); err != nil {
        errno := err.(syscall.Errno)
        if errno == syscall.EINVAL {
            log.Printf("error unmounting invalid mount %s: %s", mountPoint, err.Error())
        } else {
            return fmt.Errorf("error unmounting %s: %s", mountPoint, err.Error())
        }
    }

    if err := os.Remove(mountPoint); err != nil {
        log.Printf("error removing mountpoint %s: %s", mountPoint, err.Error())
    }

    return nil
}

func (driver *glusterDriver) Remove(req *volume.RemoveRequest) error {
    driver.mutex.Lock()
    defer driver.mutex.Unlock()

    volumeDeleteCommands := [][]string {
        {"gluster", "volume", "stop", req.Name, "--mode=script"},
        {"gluster", "volume", "delete", req.Name, "--mode=script"},
    }
    for _, cmd := range volumeDeleteCommands {
        if out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
            return fmt.Errorf("error removing gluster volume: %s [%s]", string(out), strings.Join(cmd, " "))
        }
    }

    return nil
}

func (driver *glusterDriver) Path(req *volume.PathRequest) (*volume.PathResponse, error) {
    driver.mutex.RLock()
    defer driver.mutex.RUnlock()

    if exists, err := driver.VolumeExists(req.Name); err != nil {
        return &volume.PathResponse{}, err
    } else if !exists {
        return &volume.PathResponse{}, fmt.Errorf("volume %s does not exist", req.Name)
    }

    return &volume.PathResponse{Mountpoint: path.Join(volume.DefaultDockerRootDirectory, req.Name)}, nil
}

func (driver *glusterDriver) VolumeExists(name string) (bool, error) {
    glusterVolumes, err := driver.ListVolumes()
    if err != nil {
        return false, err
    }
    for _, elem := range glusterVolumes {
        if name == elem {
            return true, nil
        }
    }
    return false, nil
}

func (driver *glusterDriver) ListVolumes() ([]string, error) {
    out, err := exec.Command("gluster", "volume", "list").CombinedOutput()
    if err != nil {
        return []string {}, fmt.Errorf("error listing gluster vols: %s", string(out))
    }
    return strings.Fields(string(out)), nil
}

func (driver *glusterDriver) ServeUnix() {
    handler := volume.NewHandler(driver)
    if err := handler.ServeUnix(driver.socket, 0); err != nil {
        log.Fatal(err)
    }
}

func initDriver() *glusterDriver {
    var hosts []string
    if os.Getenv("hosts") != "" {
        hosts = strings.Split(os.Getenv("hosts"), ",")
    }
    disk := os.Getenv("disk")
    socket := os.Getenv("socket")

    os.MkdirAll(volume.DefaultDockerRootDirectory, 0755)

    return &glusterDriver{
        disk: disk,
        hosts: hosts,
        mutex: &sync.RWMutex{},
        scope: "global",
        socket: socket,
    }
}

func main() {
    log.SetFlags(0)
    d := initDriver()
    d.ServeUnix()
}

// TODO:
// - Volume removal doesn't purge data, and you can't recreate a deleted volume
// - Gluster vol gets mounted twice
