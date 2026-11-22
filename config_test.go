package main

import (
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	cfg, err := loadConfig(env(map[string]string{"SHELF_CATEGORIES": " /comics/marvel/ :: manga "}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.categories, "|") != "comics/marvel|manga" {
		t.Errorf("categories %q", cfg.categories)
	}
	if cfg.root != "/library" || cfg.dataDir != "/data" || cfg.listen != ":25600" || cfg.cacheMax != 1024<<20 ||
		cfg.credentials != "/data/credentials" || cfg.cacheDir != "/data/cache" || cfg.seriesDir != "/data/series" {
		t.Errorf("defaults: %+v", cfg)
	}

	cfg, err = loadConfig(env(map[string]string{
		"SHELF_CATEGORIES": "c", "SHELF_LISTEN": "127.0.0.1:1", "SHELF_CACHE_MAX_MB": "5", "SHELF_AUTH": "OFF",
	}))
	if err != nil || cfg.listen != "127.0.0.1:1" || cfg.cacheMax != 5<<20 || cfg.credentials != "" {
		t.Errorf("set: %+v, %v", cfg, err)
	}

	for name, m := range map[string]map[string]string{
		"no categories":     {"SHELF_CATEGORIES": " : / "},
		"cache 0":           {"SHELF_CATEGORIES": "c", "SHELF_CACHE_MAX_MB": "0"},
		"cache not number":  {"SHELF_CATEGORIES": "c", "SHELF_CACHE_MAX_MB": "1GB"},
		"cache overflowing": {"SHELF_CATEGORIES": "c", "SHELF_CACHE_MAX_MB": "8796093022208"},
		"auth unknown":      {"SHELF_CATEGORIES": "c", "SHELF_AUTH": "false"},
	} {
		if _, err := loadConfig(env(m)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
