package doctype

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Engine writes a Date default of "today" as DEFAULT CURRENT_DATE without
// parentheses, which MySQL refuses at table creation, and the parser accepts it.
// Every maintained pack at the top of config/ (config/v0 holds legacy examples)
// is checked, so a new pack is covered without editing this test.
func TestMaintainedPacksAvoidDateDefaultsThatBreakMySQL(t *testing.T) {
	root := filepath.Join("..", "..", "config")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "v0" {
			continue
		}
		recordTypes, err := ParseConfigTree(filepath.Join(root, entry.Name()))
		if err != nil {
			continue // not a pack directory the tree parser reads; other tests cover it
		}
		checked++
		for _, dt := range recordTypes {
			for _, f := range dt.Fields {
				if f.Fieldtype == "Date" && strings.EqualFold(strings.TrimSpace(f.Default), "today") {
					t.Errorf("%s: %s.%s has a Date default of \"today\", which breaks table creation on MySQL", entry.Name(), dt.Name, f.Fieldname)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no pack was checked")
	}
}
