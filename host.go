package main

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func prettyJSON(v any) string {
	raw, errMarshal := json.MarshalIndent(v, "", "  ")
	if errMarshal != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(raw)
}

func callHostAuthList() (authListResponse, error) {
	result, errCall := callHostFunc(pluginabi.MethodHostAuthList, map[string]any{})
	if errCall != nil {
		return authListResponse{}, errCall
	}
	var resp authListResponse
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		return authListResponse{}, fmt.Errorf("decode host.auth.list result: %w", errUnmarshal)
	}
	return resp, nil
}

func callHostAuthGet(authIndex string) (authGetResponse, error) {
	result, errCall := callHostFunc(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if errCall != nil {
		return authGetResponse{}, errCall
	}
	var resp authGetResponse
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		return authGetResponse{}, fmt.Errorf("decode host.auth.get result: %w", errUnmarshal)
	}
	return resp, nil
}

func callHostAuthSave(name string, rawJSON json.RawMessage) (pluginapi.HostAuthSaveResponse, error) {
	result, errCall := callHostFunc(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: rawJSON})
	if errCall != nil {
		return pluginapi.HostAuthSaveResponse{}, errCall
	}
	var response pluginapi.HostAuthSaveResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		return pluginapi.HostAuthSaveResponse{}, fmt.Errorf("decode host.auth.save result: %w", errUnmarshal)
	}
	return response, nil
}
