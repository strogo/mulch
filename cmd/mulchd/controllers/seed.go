package controllers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/OnitiFR/mulch/cmd/mulchd/server"
	"github.com/OnitiFR/mulch/common"
)

// ListSeedController lists seeds
func ListSeedController(req *server.Request) {
	req.Response.Header().Set("Content-Type", "application/json")

	var retData common.APISeedListEntries

	for _, name := range req.App.Seeder.GetNames() {
		seed, err := req.App.Seeder.GetByName(name)
		if err != nil {
			msg := fmt.Sprintf("Seed '%s': %s", name, err)
			req.App.Log.Error(msg)
			http.Error(req.Response, msg, 500)
			return
		}

		retData = append(retData, common.APISeedListEntry{
			Name:         name,
			Ready:        seed.Ready,
			Size:         seed.Size,
			LastModified: seed.LastModified,
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
}

// GetSeedStatusController is in charge of seed status command
func GetSeedStatusController(req *server.Request) {
	seedName := req.SubPath

	if seedName == "" {
		msg := fmt.Sprintf("no seed name given")
		req.App.Log.Error(msg)
		http.Error(req.Response, msg, 400)
		return
	}

	seed, err := req.App.Seeder.GetByName(seedName)
	if err != nil {
		msg := fmt.Sprintf("seed '%s' not found", seedName)
		req.App.Log.Error(msg)
		http.Error(req.Response, msg, 404)
		return
	}

	data := &common.APISeedStatus{
		Name:       seedName,
		As:         seed.As,
		Ready:      seed.Ready,
		CurrentURL: seed.CurrentURL,
		Size:       seed.Size,
		Status:     seed.Status,
		StatusTime: seed.StatusTime,
	}

	req.Response.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(req.Response)
	err = enc.Encode(data)
	if err != nil {
		req.App.Log.Error(err.Error())
		http.Error(req.Response, err.Error(), 500)
	}
}
