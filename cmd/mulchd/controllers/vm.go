package controllers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"time"

	"github.com/OnitiFR/mulch/cmd/mulchd/server"
	"github.com/OnitiFR/mulch/common"
	"golang.org/x/crypto/ssh"
)

func getEntryFromRequest(vmName string, req *server.Request) (*server.VMDatabaseEntry, error) {
	var entry *server.VMDatabaseEntry
	var err error

	action := req.HTTP.FormValue("action")
	revisionParams := req.HTTP.FormValue("revision")

	if action == "activate" && (revisionParams == "none" || revisionParams == "-1") {
		count := req.App.VMDB.GetCountForName(vmName)
		if count == 0 {
			return nil, fmt.Errorf("no %s VM found in database", vmName)
		}
		// create a fake entry
		entry = &server.VMDatabaseEntry{
			Name:   server.NewVMName(vmName, server.RevisionNone),
			VM:     nil,
			Active: false,
		}
	} else if revisionParams != "" {
		revision, err := strconv.Atoi(revisionParams)
		if err != nil {
			return nil, err
		}
		entry, err = req.App.VMDB.GetEntryByName(server.NewVMName(vmName, revision))
		if err != nil {
			return nil, err
		}
	} else {
		entry, err = req.App.VMDB.GetActiveEntryByName(vmName)
		if err != nil {
			return nil, err
		}
	}

	return entry, nil
}

// VMControllerConfigCheck will validate TOML sent in the 'config' request field
// and check if VM is a duplicate
func VMControllerConfigCheck(req *server.Request) (*server.VMConfig, string, error) {
	configFile, header, err := req.HTTP.FormFile("config")
	if err != nil {
		return nil, "", fmt.Errorf("'config' file field: %s", err)
	}
	filename := header.Filename

	conf, err := server.NewVMConfigFromTomlReader(configFile, req.Stream)
	if err != nil {
		return nil, "", fmt.Errorf("decoding config: %s", err)
	}

	allowNewRevision := req.HTTP.FormValue("allow_new_revision")

	if req.App.VMDB.GetCountForName(conf.Name) > 0 && allowNewRevision != common.TrueStr {
		return nil, "", fmt.Errorf("VM '%s' already exists (see --new-revision CLI option?)", conf.Name)
	}

	return conf, filename, nil
}

// NewVMAsyncController creates asynchronously a new VM and
// use a callback URL when finished (success or failure)
func NewVMAsyncController(req *server.Request) {
	req.Response.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(req.Response)

	callbackURL := req.HTTP.FormValue("callback_url")

	_, err := url.ParseRequestURI(callbackURL)
	if err != nil {
		req.Response.WriteHeader(http.StatusBadRequest)
		enc.Encode("Invalid or empty callback_url parameter")
		return
	}

	_, _, err = VMControllerConfigCheck(req)
	if err != nil {
		req.Response.WriteHeader(http.StatusBadRequest)
		enc.Encode(err.Error())
		return
	}

	go func() {
		before := time.Now()
		vm, err := NewVMController(req)
		after := time.Now()

		name := "unknown"
		if vm != nil {
			name = vm.Config.Name
		}

		data := common.AsyncCallback{
			Action:   "create",
			Target:   name,
			Success:  true,
			Duration: after.Sub(before),
		}
		if err != nil {
			data.Success = false
			data.Error = err.Error()
		}

		client := http.Client{
			Timeout: time.Duration(30 * time.Second),
		}
		_, err = client.PostForm(callbackURL, data.AsURLValue())
		if err != nil {
			req.App.Log.Errorf("unable to contact callback URL '%s' for VM %s: %s", callbackURL, name, err.Error())
		}
	}()

	str := fmt.Sprintf("OK. VM creation started, will contact %s", callbackURL)
	enc.Encode(str)
}

// NewVMSyncController creates synchronously a new VM
// (we just remove error from NewVMController, since we use the stream
// to report errors)
func NewVMSyncController(req *server.Request) {
	NewVMController(req)
}

// NewVMController creates a new VM
func NewVMController(req *server.Request) (*server.VM, error) {
	req.StartStream()

	conf, filename, err := VMControllerConfigCheck(req)
	if err != nil {
		req.Stream.Failure(err.Error())
		return nil, err
	}
	req.Stream.Tracef("reading '%s' config file", filename)

	restore := req.HTTP.FormValue("restore")
	restoreVM := req.HTTP.FormValue("restore-vm")
	inactive := req.HTTP.FormValue("inactive")
	keepOnFailure := req.HTTP.FormValue("keep_on_failure")
	lock := req.HTTP.FormValue("lock")

	active := true
	if inactive == common.TrueStr {
		active = false
	}

	allowScriptFailure := server.VMStopOnScriptFailure
	if keepOnFailure == common.TrueStr {
		allowScriptFailure = server.VMAllowScriptFailure
	}

	if restore != "" && restoreVM != "" {
		msg := "restore and restore-vm flags are mutually exclusive"
		req.Stream.Failure(msg)
		return nil, errors.New(msg)
	}

	operation := req.App.Operations.Add(&server.Operation{
		Origin:        req.APIKey.Comment,
		Action:        "create",
		Ressource:     "vm",
		RessourceName: conf.Name,
	})
	defer req.App.Operations.Remove(operation)

	req.SetTarget(conf.Name)

	// restore from an existing backup
	if restore != "" {
		conf.RestoreBackup = restore
		req.Stream.Infof("will restore VM from '%s'", restore)
	}

	// restore from a new backup
	if restoreVM != "" {
		entry, err := req.App.VMDB.GetActiveEntryByName(restoreVM)
		if err != nil {
			msg := fmt.Sprintf("Cannot find VM to backup: %s", err)
			req.Stream.Failuref(msg)
			return nil, errors.New(msg)
		}
		backup, err := server.VMBackup(entry.Name, req.APIKey.Comment, req.App, req.Stream, server.BackupCompressDisable)
		if err != nil {
			msg := fmt.Sprintf("Cannot backup: %s", err)
			req.Stream.Failuref(msg)
			return nil, errors.New(msg)
		}
		defer func() {
			err := deleteBackup(backup, req)
			if err != nil {
				req.App.Log.Errorf("cannot delete transient backup: %s", err)
			}
		}()
		conf.RestoreBackup = backup
	}

	before := time.Now()
	vm, vmName, err := server.NewVM(conf, active, allowScriptFailure, req.APIKey.Comment, req.App, req.Stream)
	if err != nil {
		msg := fmt.Sprintf("Cannot create VM: %s", err)
		req.Stream.Failuref(msg)
		return nil, errors.New(msg)
	}

	if lock == common.TrueStr {
		err = server.VMLockUnlock(vmName, true, req.App.VMDB)
		if err != nil {
			// non-fatal
			req.Stream.Errorf("Cannot lock the VM: %s", err)
		}
	}

	after := time.Now()

	req.Stream.Successf("VM %s created successfully (%s)", vmName, after.Sub(before))
	return vm, nil
}

// ListVMsController list VMs
func ListVMsController(req *server.Request) {
	req.Response.Header().Set("Content-Type", "application/json")

	basicListing := false
	if req.HTTP.FormValue("basic") == common.TrueStr {
		basicListing = true
	}

	vmNames := req.App.VMDB.GetNames()

	if basicListing {
		var retData common.APIVMBasicListEntries
		for _, vmName := range vmNames {
			retData = append(retData, common.APIVMBasicListEntry{
				Name: vmName.Name,
			})
		}
		sort.Slice(retData, func(i, j int) bool {
			return retData[i].Name < retData[j].Name
		})

		enc := json.NewEncoder(req.Response)
		err := enc.Encode(&retData)
		if err != nil {
			req.App.Log.Error(err.Error())
			http.Error(req.Response, err.Error(), 500)
		}
	} else {

		var retData common.APIVMListEntries
		for _, vmName := range vmNames {
			vm, err := req.App.VMDB.GetByName(vmName)
			if err != nil {
				msg := fmt.Sprintf("VM %s: %s", vmName, err)
				req.App.Log.Error(msg)
				http.Error(req.Response, msg, 500)
				return
			}

			domain, err := req.App.Libvirt.GetDomainByName(vmName.LibvirtDomainName(req.App))
			if err != nil {
				msg := fmt.Sprintf("VM %s: %s", vmName, err)
				req.App.Log.Error(msg)
				http.Error(req.Response, msg, 500)
				return
			}
			if domain == nil {
				msg := fmt.Sprintf("VM %s: does not exists in libvirt", vmName)
				req.App.Log.Error(msg)
				http.Error(req.Response, msg, 500)
				return
			}
			defer domain.Free()

			state, _, err := domain.GetState()
			if err != nil {
				msg := fmt.Sprintf("VM %s: %s", vmName, err)
				req.App.Log.Error(msg)
				http.Error(req.Response, msg, 500)
				return
			}

			active, err := req.App.VMDB.IsVMActive(vmName)
			if err != nil {
				msg := fmt.Sprintf("VM %s: %s", vmName, err)
				req.App.Log.Error(msg)
				http.Error(req.Response, msg, 500)
				return
			}

			// if state == libvirt.DOMAIN_RUNNING {
			// 	// check if services are running? (SSH? port?)
			// }

			retData = append(retData, common.APIVMListEntry{
				Name:      vmName.Name,
				Revision:  vmName.Revision,
				Active:    active,
				LastIP:    vm.LastIP,
				State:     server.LibvirtDomainStateToString(state),
				Locked:    vm.Locked,
				WIP:       string(vm.WIP),
				SuperUser: vm.App.Config.MulchSuperUser,
				AppUser:   vm.Config.AppUser,
			})
		}

		sort.Slice(retData, func(i, j int) bool {
			if retData[i].Name == retData[j].Name {
				return retData[i].Revision < retData[j].Revision
			}
			return retData[i].Name < retData[j].Name
		})

		enc := json.NewEncoder(req.Response)
		err := enc.Encode(&retData)
		if err != nil {
			req.App.Log.Error(err.Error())
			http.Error(req.Response, err.Error(), 500)
		}
	}
}

// ActionVMController redirect to the correct action for the VM (start/stop/…)
func ActionVMController(req *server.Request) {
	vmName := req.SubPath

	if vmName == "" {
		req.StartStream()
		req.Stream.Failuref("invalid VM name")
		return
	}

	entry, err := getEntryFromRequest(vmName, req)
	if err != nil {
		req.StartStream()
		req.Stream.Failure(err.Error())
		return
	}

	vm := entry.VM
	req.Response.Header().Set("Current-VM-Name", entry.Name.ID())
	req.StartStream()

	action := req.HTTP.FormValue("action")
	operationAction := action

	if action != "do" {
		// 'do' actions can send "private" special messages to client (like
		// _MULCH_OPEN_URL) so don't broadcast output to vmName target
		req.SetTarget(vmName)
	} else {
		operationAction = "do:" + req.HTTP.FormValue("do_action")
	}

	operation := req.App.Operations.Add(&server.Operation{
		Origin:        req.APIKey.Comment,
		Action:        operationAction,
		Ressource:     "vm",
		RessourceName: entry.Name.ID(),
	})
	defer req.App.Operations.Remove(operation)

	switch action {
	case "lock":
		if vm.Locked {
			req.Stream.Warningf("%s already locked", entry.Name)
		}
		err := server.VMLockUnlock(entry.Name, true, req.App.VMDB)
		if err != nil {
			req.Stream.Failuref("unable to lock %s: %s", entry.Name, err)
		} else {
			req.Stream.Successf("%s is now locked", entry.Name)
		}
	case "unlock":
		if vm.Locked == false {
			req.Stream.Warningf("%s already unlocked", entry.Name)
		}
		err := server.VMLockUnlock(entry.Name, false, req.App.VMDB)
		if err != nil {
			req.Stream.Failuref("unable to unlock %s: %s", entry.Name, err)
		} else {
			req.Stream.Successf("%s is now unlocked", entry.Name)
		}
	case "start":
		req.Stream.Infof("starting %s", vmName)
		err := server.VMStartByName(entry.Name, vm.SecretUUID, req.App, req.Stream)
		if err != nil {
			req.Stream.Failuref("unable to start %s: %s", entry.Name, err)
		} else {
			req.Stream.Successf("VM %s is now up and running", entry.Name)
		}
	case "stop":
		req.Stream.Infof("stopping %s", vmName)
		err := server.VMStopByName(entry.Name, req.App, req.Stream)
		if err != nil {
			req.Stream.Failuref("unable to stop %s: %s", entry.Name, err)
		} else {
			req.Stream.Successf("VM %s is now down", entry.Name)
		}
	case "exec":
		err := ExecScriptVM(req, vm, entry.Name)
		if err != nil {
			req.Stream.Failuref("error: %s", err)
		}
	case "do":
		err := DoActionVM(req, vm, entry.Name)
		if err != nil {
			req.Stream.Failuref("error: %s", err)
		}
	case "backup":
		volHame, err := BackupVM(req, entry.Name)
		if err != nil {
			req.Stream.Failuref("error: %s", err)
		} else {
			req.Stream.Successf("backup completed (%s)", volHame)
		}
	case "rebuild":
		before := time.Now()
		err := RebuildVMv2(req, vm, entry.Name)
		after := time.Now()
		if err != nil {
			req.Stream.Failuref("error: %s", err)
		} else {
			req.Stream.Successf("rebuild completed (%s)", after.Sub(before))
		}
	case "redefine":
		err := RedefineVM(req, vm, entry.Active)
		if err != nil {
			req.Stream.Failuref("error: %s", err)
		} else {
			req.Stream.Successf("VM %s redefined (may the sysadmin gods be with you)", entry.Name)
		}
	case "activate":
		err := req.App.VMDB.SetActiveRevision(entry.Name.Name, entry.Name.Revision)
		if err != nil {
			req.Stream.Failuref("error: %s", err)
		} else {
			if entry.Name.Revision != server.RevisionNone {
				req.Stream.Successf("VM %s is now active", entry.Name)
			} else {
				req.Stream.Successf("VM %s is now inactive", entry.Name.Name)
			}
		}
	default:
		req.Stream.Failuref("missing or invalid action ('%s') for '%s'", action, vmName)
		return
	}
}

// DeleteVMController will delete a (unlocked) VM
func DeleteVMController(req *server.Request) {
	req.StartStream()
	vmName := req.SubPath
	req.SetTarget(vmName)

	entry, err := getEntryFromRequest(vmName, req)
	if err != nil {
		req.Stream.Failure(err.Error())
		return
	}

	operation := req.App.Operations.Add(&server.Operation{
		Origin:        req.APIKey.Comment,
		Action:        "delete",
		Ressource:     "vm",
		RessourceName: entry.Name.ID(),
	})
	defer req.App.Operations.Remove(operation)

	req.Stream.Infof("deleting vm %s", entry.Name)

	err = server.VMDelete(entry.Name, req.App, req.Stream)
	if err != nil {
		req.Stream.Failuref("unable to delete VM '%s': %s", vmName, err)
	} else {
		req.Stream.Successf("VM '%s' successfully deleted", vmName)
	}
}

// ExecScriptVM will execute a script inside the VM
func ExecScriptVM(req *server.Request, vm *server.VM, vmName *server.VMName) error {
	script, header, err := req.HTTP.FormFile("script")
	if err != nil {
		return fmt.Errorf("'script' field: %s", err)
	}

	operation := req.App.Operations.Add(&server.Operation{
		Origin:        req.APIKey.Comment,
		Action:        "exec",
		Ressource:     "vm",
		RessourceName: vmName.ID(),
	})
	defer req.App.Operations.Remove(operation)

	running, _ := server.VMIsRunning(vmName, req.App)
	if running == false {
		return errors.New("VM should be up and running")
	}

	as := req.HTTP.FormValue("as")

	SSHSuperUserAuth, err := req.App.SSHPairDB.GetPublicKeyAuth(vm.MulchSuperUserSSHKey)
	if err != nil {
		return err
	}

	run := &server.Run{
		SSHConn: &server.SSHConnection{
			User: vm.App.Config.MulchSuperUser,
			Host: vm.LastIP,
			Port: 22,
			Auths: []ssh.AuthMethod{
				SSHSuperUserAuth,
			},
			Log: req.Stream,
		},
		Tasks: []*server.RunTask{
			&server.RunTask{
				ScriptName:   header.Filename,
				ScriptReader: script,
				As:           as,
			},
		},
		Log:          req.Stream,
		CloseChannel: req.Response.(http.CloseNotifier).CloseNotify(),
	}
	err = run.Go()
	if err != nil {
		return err
	}

	req.Stream.Successf("script '%s' returned 0", header.Filename)
	return nil
}

// DoActionVM will execute a "do action" in the VM
func DoActionVM(req *server.Request, vm *server.VM, vmName *server.VMName) error {
	actionName := req.HTTP.FormValue("do_action")
	arguments := req.HTTP.FormValue("arguments")

	action, exists := vm.Config.DoActions[actionName]
	if !exists {
		return fmt.Errorf("unable to find action '%s' for %s", actionName, vmName)
	}

	running, _ := server.VMIsRunning(vmName, req.App)
	if running == false {
		return errors.New("VM should be up and running")
	}

	stream, errG := server.GetContentFromURL(action.ScriptURL)
	if errG != nil {
		return fmt.Errorf("unable to get script '%s': %s", action.ScriptURL, errG)
	}
	defer stream.Close()

	SSHSuperUserAuth, err := req.App.SSHPairDB.GetPublicKeyAuth(vm.MulchSuperUserSSHKey)
	if err != nil {
		return err
	}

	before := time.Now()

	run := &server.Run{
		Caption: "do",
		SSHConn: &server.SSHConnection{
			User: vm.App.Config.MulchSuperUser,
			Host: vm.LastIP,
			Port: 22,
			Auths: []ssh.AuthMethod{
				SSHSuperUserAuth,
			},
			Log: req.Stream,
		},
		Tasks: []*server.RunTask{
			&server.RunTask{
				ScriptName:   path.Base(action.ScriptURL),
				ScriptReader: stream,
				As:           action.User,
				Arguments:    arguments,
			},
		},
		Log:          req.Stream,
		CloseChannel: req.Response.(http.CloseNotifier).CloseNotify(),
	}
	err = run.Go()
	if err != nil {
		return err
	}

	after := time.Now()

	req.Stream.Successf("script returned 0 (%s)", after.Sub(before))
	return nil
}

// GetVMConfigController return a VM config file content
func GetVMConfigController(req *server.Request) {
	vmName := req.SubPath

	if vmName == "" {
		msg := fmt.Sprintf("no VM name given")
		req.App.Log.Error(msg)
		http.Error(req.Response, msg, 400)
		return
	}
	entry, err := getEntryFromRequest(vmName, req)
	if err != nil {
		req.App.Log.Error(err.Error())
		http.Error(req.Response, err.Error(), 404)
		return
	}

	req.Response.Header().Set("Content-Type", "text/plain")
	req.Println(entry.VM.Config.FileContent)
}

// GetVMInfosController return VM informations
func GetVMInfosController(req *server.Request) {
	vmName := req.SubPath

	if vmName == "" {
		msg := fmt.Sprintf("no VM name given")
		req.App.Log.Error(msg)
		http.Error(req.Response, msg, 400)
		return
	}

	entry, err := getEntryFromRequest(vmName, req)
	if err != nil {
		req.App.Log.Error(err.Error())
		http.Error(req.Response, err.Error(), 404)
		return
	}

	vm := entry.VM

	running, _ := server.VMIsRunning(entry.Name, req.App)

	diskName, err := server.VMGetDiskName(entry.Name, req.App)
	if err != nil {
		req.App.Log.Error(err.Error())
		http.Error(req.Response, err.Error(), 500)
		return
	}
	vInfos, err := req.App.Libvirt.VolumeInfos(diskName, req.App.Libvirt.Pools.Disks)
	if err != nil {
		req.App.Log.Error(err.Error())
		http.Error(req.Response, err.Error(), 500)
		return
	}

	var domains []string
	for _, domain := range vm.Config.Domains {
		domains = append(domains, domain.Name)
	}

	data := &common.APIVMInfos{
		Name:                entry.Name.Name,
		Revision:            entry.Name.Revision,
		Up:                  running,
		Active:              entry.Active,
		Seed:                vm.Config.Seed,
		CPUCount:            vm.Config.CPUCount,
		RAMSizeMB:           (vm.Config.RAMSize / 1024 / 1024),
		DiskSizeMB:          (vm.Config.DiskSize / 1024 / 1024),
		AllocatedDiskSizeMB: (vInfos.Allocation / 1024 / 1024),
		BackupDiskSizeMB:    (vm.Config.BackupDiskSize / 1024 / 1024),
		Hostname:            vm.Config.Hostname,
		Domains:             domains,
		SuperUser:           vm.App.Config.MulchSuperUser,
		AppUser:             vm.Config.AppUser,
		AuthorKey:           vm.AuthorKey,
		InitDate:            vm.InitDate,
		LastRebuildDuration: vm.LastRebuildDuration,
		LastRebuildDowntime: vm.LastRebuildDowntime,
		Locked:              vm.Locked,
		AssignedIPv4:        vm.AssignedIPv4,
		AssignedMAC:         vm.AssignedMAC,
	}

	req.Response.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(req.Response)
	err = enc.Encode(data)
	if err != nil {
		req.App.Log.Error(err.Error())
		http.Error(req.Response, err.Error(), 500)
	}
}

// GetVMDoActionsController return VM do-action list
func GetVMDoActionsController(req *server.Request) {
	req.Response.Header().Set("Content-Type", "application/json")

	vmName := req.SubPath

	if vmName == "" {
		msg := fmt.Sprintf("no VM name given")
		req.App.Log.Error(msg)
		http.Error(req.Response, msg, 400)
		return
	}

	entry, err := getEntryFromRequest(vmName, req)
	if err != nil {
		msg := fmt.Sprintf("VM '%s' not found", vmName)
		req.App.Log.Error(msg)
		http.Error(req.Response, msg, 404)
		return
	}
	vm := entry.VM

	var retData common.APIVMDoListEntries

	for _, action := range vm.Config.DoActions {
		retData = append(retData, common.APIVMDoListEntry{
			Name:        action.Name,
			User:        action.User,
			Description: action.Description,
		})
	}

	sort.Slice(retData, func(i, j int) bool {
		return retData[i].Name < retData[j].Name
	})

	enc := json.NewEncoder(req.Response)
	err = enc.Encode(&retData)
	if err != nil {
		req.App.Log.Error(err.Error())
		http.Error(req.Response, err.Error(), 500)
	}
}

// BackupVM launch the backup process
func BackupVM(req *server.Request, vmName *server.VMName) (string, error) {
	return server.VMBackup(vmName, req.APIKey.Comment, req.App, req.Stream, server.BackupCompressAllow)
}

// RebuildVMv2 delete VM and rebuilds it from a backup (2nd version, using revisions)
func RebuildVMv2(req *server.Request, vm *server.VM, vmName *server.VMName) error {

	if vm.Locked == true && req.HTTP.FormValue("force") != common.TrueStr {
		return errors.New("VM is locked (see --force)")
	}

	lock := req.HTTP.FormValue("lock")

	return server.VMRebuild(vmName, lock == common.TrueStr, req.APIKey.Comment, req.App, req.Stream)
}

// RedefineVM replace VM config file with a new one, for next rebuild
func RedefineVM(req *server.Request, vm *server.VM, active bool) error {
	if vm.Locked == true && req.HTTP.FormValue("force") != common.TrueStr {
		return errors.New("VM is locked (see --force)")
	}

	if vm.WIP != server.VMOperationNone {
		return fmt.Errorf("VM have a work in progress (%s)", string(vm.WIP))
	}

	configFile, header, err := req.HTTP.FormFile("config")
	if err != nil {
		return fmt.Errorf("'config' file field: %s", err)
	}
	req.Stream.Tracef("reading '%s' config file", header.Filename)

	conf, err := server.NewVMConfigFromTomlReader(configFile, req.Stream)
	if err != nil {
		return fmt.Errorf("decoding config: %s", err)
	}

	if conf.Name != vm.Config.Name {
		return fmt.Errorf("VM name does not match")
	}

	if active {
		err = server.CheckDomainsConflicts(req.App.VMDB, conf.Domains, conf.Name, req.App.Config)
		if err != nil {
			return err
		}
	}

	// change author
	vm.AuthorKey = req.APIKey.Comment

	oldActions := vm.Config.DoActions

	// redefine config
	vm.Config = conf

	// re-add old 'from prepare' actions (only if a new 'from config' action with
	// the same name is not already defined)
	for name, action := range oldActions {
		if action.FromConfig == true {
			continue
		}
		if _, exists := vm.Config.DoActions[name]; exists {
			req.Stream.Warningf("new action '%s' will replace the old one", name)
			continue
		}
		vm.Config.DoActions[name] = action
	}

	req.App.VMDB.Update()
	if err != nil {
		return err
	}

	return nil
}
