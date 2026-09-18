package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type proxyMapFile struct {
	Accounts map[string]proxyMapEntry `json:"accounts"`
}

type proxyMapEntry struct {
	Email    string `json:"email,omitempty"`
	ProxyURL string `json:"proxy_url"`
}

func (g *quotaGuard) backgroundProxyRestoreLoop(interval time.Duration, stop <-chan struct{}) {
	g.reconcileAuthProxies()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			g.reconcileAuthProxies()
		case <-stop:
			return
		}
	}
}

func (g *quotaGuard) reconcileAuthProxies() {
	g.mu.Lock()
	cfg := g.cfg
	g.mu.Unlock()
	if !cfg.ProxyRestoreEnabled {
		return
	}
	mappings, errLoad := loadProxyMap(cfg.ProxyMapFile)
	if errLoad != nil {
		g.recordProxyResult(nil, "map error", "", errLoad)
		return
	}
	auths, errList := callHostAuthList()
	if errList != nil {
		g.recordProxyResult(nil, "host error", "", errList)
		return
	}
	for _, file := range auths.Files {
		if !isCodexAuth(file) || file.RuntimeOnly || strings.TrimSpace(file.AuthIndex) == "" {
			continue
		}
		g.reconcileOneProxy(file, mappings, cfg)
	}
}

func loadProxyMap(name string) (proxyMapFile, error) {
	path := strings.TrimSpace(name)
	if path == "" {
		return proxyMapFile{}, fmt.Errorf("proxy map file is empty")
	}
	if !filepath.IsAbs(path) {
		if abs, errAbs := filepath.Abs(path); errAbs == nil {
			path = abs
		}
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return proxyMapFile{}, fmt.Errorf("read proxy map: %w", errRead)
	}
	var mappings proxyMapFile
	if errDecode := json.Unmarshal(raw, &mappings); errDecode != nil {
		return proxyMapFile{}, fmt.Errorf("decode proxy map: %w", errDecode)
	}
	if mappings.Accounts == nil {
		mappings.Accounts = map[string]proxyMapEntry{}
	}
	return mappings, nil
}

func (g *quotaGuard) reconcileOneProxy(file pluginapi.HostAuthFileEntry, mappings proxyMapFile, cfg pluginConfig) {
	g.mu.Lock()
	account := g.ensureAccountByKeyLocked(strings.TrimSpace(file.ID))
	account.ProxyLastCheckedAt = g.now()
	g.mu.Unlock()

	raw, errGet := callHostAuthGet(file.AuthIndex)
	if errGet != nil {
		entry, _ := proxyMappingForFile(file, mappings)
		g.recordProxyResult(&file, "error", proxyDisplayURL(entry.ProxyURL), errGet)
		return
	}
	var metadata map[string]any
	if errDecode := json.Unmarshal(raw.JSON, &metadata); errDecode != nil {
		entry, _ := proxyMappingForFile(file, mappings)
		g.recordProxyResult(&file, "error", proxyDisplayURL(entry.ProxyURL), errDecode)
		return
	}
	current, _ := metadata["proxy_url"].(string)
	if strings.TrimSpace(current) != "" {
		entry, key := proxyMappingForFile(file, mappings)
		if strings.TrimSpace(entry.ProxyURL) == "" {
			// A newly added auth may already contain its proxy. Learn its
			// email mapping for a future OAuth re-login.
			errLearn := learnProxyMapping(file, current, mappings, cfg.ProxyMapFile)
			g.recordProxyResult(&file, "configured", proxyDisplayURL(current), errLearn)
			return
		}
		if strings.TrimSpace(current) == strings.TrimSpace(entry.ProxyURL) {
			g.recordProxyResult(&file, "restored", proxyDisplayURL(current), nil)
		} else {
			g.recordProxyResult(&file, "mismatch", proxyDisplayURL(current), fmt.Errorf("mapped proxy differs for %s", key))
		}
		return
	}
	entry, _ := proxyMappingForFile(file, mappings)
	if strings.TrimSpace(entry.ProxyURL) == "" {
		g.recordProxyResult(&file, "missing mapping", "", nil)
		return
	}
	if cfg.ProxyRestoreSettleSecs > 0 && !file.ModTime.IsZero() && g.now().Sub(file.ModTime) < time.Duration(cfg.ProxyRestoreSettleSecs)*time.Second {
		g.recordProxyResult(&file, "waiting", proxyDisplayURL(entry.ProxyURL), nil)
		return
	}
	metadata["proxy_url"] = strings.TrimSpace(entry.ProxyURL)
	updated, errMarshal := json.Marshal(metadata)
	if errMarshal != nil {
		g.recordProxyResult(&file, "error", proxyDisplayURL(entry.ProxyURL), errMarshal)
		return
	}
	if _, errSave := callHostAuthSave(firstNonEmpty(raw.Name, file.Name), updated); errSave != nil {
		g.recordProxyResult(&file, "error", proxyDisplayURL(entry.ProxyURL), errSave)
		return
	}
	g.recordProxyResult(&file, "restored", proxyDisplayURL(entry.ProxyURL), nil)
}

func learnProxyMapping(file pluginapi.HostAuthFileEntry, proxyURL string, mappings proxyMapFile, path string) error {
	email := strings.ToLower(strings.TrimSpace(file.Email))
	if email == "" {
		return nil
	}
	if existing, ok := mappings.Accounts[email]; ok && strings.TrimSpace(existing.ProxyURL) != "" {
		return nil
	}
	mappings.Accounts[email] = proxyMapEntry{Email: strings.TrimSpace(file.Email), ProxyURL: strings.TrimSpace(proxyURL)}
	return saveProxyMap(path, mappings)
}

func proxyMappingForFile(file pluginapi.HostAuthFileEntry, mappings proxyMapFile) (proxyMapEntry, string) {
	// Prefer email because an account ID can be reused after re-login.
	var keys = []string{strings.TrimSpace(file.Email), strings.TrimSpace(file.Account)}
	for _, key := range keys {
		if key == "" {
			continue
		}
		if entry, ok := mappings.Accounts[key]; ok {
			if strings.TrimSpace(entry.Email) == "" || strings.EqualFold(strings.TrimSpace(entry.Email), strings.TrimSpace(file.Email)) {
				return entry, key
			}
		}
		for mappedKey, entry := range mappings.Accounts {
			if (strings.EqualFold(strings.TrimSpace(mappedKey), key) || strings.EqualFold(strings.TrimSpace(entry.Email), key)) &&
				(strings.TrimSpace(entry.Email) == "" || strings.EqualFold(strings.TrimSpace(entry.Email), strings.TrimSpace(file.Email))) {
				return entry, mappedKey
			}
		}
	}
	return proxyMapEntry{}, ""
}

func saveProxyMap(name string, mappings proxyMapFile) error {
	path := strings.TrimSpace(name)
	if path == "" {
		return fmt.Errorf("proxy map file is empty")
	}
	if !filepath.IsAbs(path) {
		if abs, errAbs := filepath.Abs(path); errAbs == nil {
			path = abs
		}
	}
	raw, errMarshal := json.MarshalIndent(mappings, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("encode proxy map: %w", errMarshal)
	}
	tmp := path + ".tmp"
	if errWrite := os.WriteFile(tmp, raw, 0o600); errWrite != nil {
		return fmt.Errorf("write proxy map: %w", errWrite)
	}
	if errRename := os.Rename(tmp, path); errRename != nil {
		return fmt.Errorf("replace proxy map: %w", errRename)
	}
	return nil
}

func proxyDisplayURL(raw string) string {
	value := strings.TrimSpace(raw)
	parsed, errParse := url.Parse(value)
	if errParse != nil || parsed.Scheme == "" {
		return "*"
	}
	port := parsed.Port()
	if port == "" {
		return parsed.Scheme + "://*"
	}
	return parsed.Scheme + "://*:" + port
}

func (g *quotaGuard) recordProxyResult(file *pluginapi.HostAuthFileEntry, status, display string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if file == nil {
		return
	}
	key := strings.TrimSpace(file.ID)
	if key == "" {
		key = strings.TrimSpace(file.AuthIndex)
	}
	account := g.ensureAccountByKeyLocked(key)
	previousStatus := account.ProxyStatus
	changed := previousStatus != status || account.ProxyDisplay != display
	account.ProxyStatus = status
	account.ProxyDisplay = display
	account.ProxyLastCheckedAt = g.now()
	account.ProxyLastError = ""
	if err != nil {
		account.ProxyLastError = err.Error()
		changed = true
	}
	if status == "restored" && previousStatus != "restored" {
		account.ProxyLastRestoredAt = g.now()
		changed = true
	}
	if changed {
		g.saveErr = g.saveStateLocked()
	}
}
