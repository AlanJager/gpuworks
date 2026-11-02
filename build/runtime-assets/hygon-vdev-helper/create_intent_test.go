package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedCreateKeepsIntentWhenConfigCannotBeVerified(t *testing.T) {
	config, record := createPaths(t)
	marker := filepath.Join(filepath.Dir(record), "vdev-create-pending")
	api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: []int{17}, info: expectedInfo, exists: true}
	creates := 0
	api.createFn = func() {
		creates++
		if err := os.WriteFile(config(17), []byte("truncated vendor config\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := runWithConfig(createArgs(record), api, &out, os.Remove, config); err == nil || out.Len() != 0 {
		t.Fatalf("unverified native create: error=%v output=%q", err, out.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("unverified creation lost its retry fence: %v", err)
	}
	if err := runWithConfig(createArgs(record), api, &out, os.Remove, config); err == nil || !strings.Contains(err.Error(), "pending") || creates != 1 {
		t.Fatalf("retry allocated another vdev: error=%v creates=%d", err, creates)
	}
	if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) || api.destroyed != 0 {
		t.Fatalf("unverified device was claimed or destroyed: record=%v destroyed=%d", err, api.destroyed)
	}
}

func TestManagedCreateKeepsIntentForAmbiguousNativeResults(t *testing.T) {
	for _, ids := range [][]int{{-1}, {17, 18}, nil} {
		config, record := createPaths(t)
		api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: ids, createErr: errors.New("native result uncertain")}
		creates := 0
		api.createFn = func() { creates++ }
		var out bytes.Buffer
		if err := runWithConfig(createArgs(record), api, &out, os.Remove, config); err == nil || out.Len() != 0 {
			t.Fatalf("ids=%v error=%v output=%q", ids, err, out.String())
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(record), "vdev-create-pending")); err != nil {
			t.Fatalf("ids=%v lost intent: %v", ids, err)
		}
		if err := runWithConfig(createArgs(record), api, &out, os.Remove, config); err == nil || creates != 1 {
			t.Fatalf("ids=%v retried uncertain create: error=%v creates=%d", ids, err, creates)
		}
	}
}

func TestManagedCreateClearsIntentOnlyAfterDurableOwnershipOrDestroy(t *testing.T) {
	for _, mode := range []string{"success", "rollback destroyed", "rollback uncertain"} {
		t.Run(mode, func(t *testing.T) {
			config, record := createPaths(t)
			marker := filepath.Join(filepath.Dir(record), "vdev-create-pending")
			api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: []int{17}, info: expectedInfo, exists: true}
			if mode == "rollback uncertain" {
				api.destroyErr = errors.New("device busy")
			}
			api.createFn = func() {
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("native create ran without durable intent: %v", err)
				}
				if err := os.WriteFile(config(17), []byte(vendorConfig(17, expectedInfo)), 0o644); err != nil {
					t.Fatal(err)
				}
				if mode != "success" {
					// Inject a persistence collision after preflight, once native
					// creation has already started and rollback is required.
					if err := os.WriteFile(record, []byte("concurrent record\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			var out bytes.Buffer
			err := runWithConfig(createArgs(record), api, &out, os.Remove, config)
			if mode == "success" {
				if err != nil || out.String() != "{\"vdev_id\":17}\n" {
					t.Fatalf("verified create: error=%v output=%q", err, out.String())
				}
				if owner, err := readCurrentOwner(filepath.Dir(config(0)), 17); err != nil || !owner.Active {
					t.Fatalf("intent cleared before durable owner: owner=%+v error=%v", owner, err)
				}
			} else if err == nil || api.destroyed != 17 || out.Len() != 0 {
				t.Fatalf("rollback result: error=%v destroyed=%d output=%q", err, api.destroyed, out.String())
			}
			_, markerErr := os.Stat(marker)
			if mode == "rollback uncertain" {
				if markerErr != nil {
					t.Fatalf("uncertain rollback lost intent: %v", markerErr)
				}
			} else if !errors.Is(markerErr, os.ErrNotExist) {
				t.Fatalf("completed creation retained intent: %v", markerErr)
			}
		})
	}
}

func TestManagedCreateRejectsExistingIntentBeforeNativeCreate(t *testing.T) {
	config, record := createPaths(t)
	marker := filepath.Join(filepath.Dir(record), "vdev-create-pending")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: []int{17}, info: expectedInfo, exists: true}
	api.createFn = func() { t.Fatal("existing intent allowed another native creation") }
	var out bytes.Buffer
	if err := runWithConfig(createArgs(record), api, &out, os.Remove, config); err == nil || api.created || out.Len() != 0 {
		t.Fatalf("pending creation was retried: error=%v created=%t output=%q", err, api.created, out.String())
	}
}

func TestManagedCreateIntentPersistenceFailureStopsBeforeNativeCreate(t *testing.T) {
	config, record := createPaths(t)
	record = filepath.Join(filepath.Dir(record), "missing", "vdev-id")
	api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: []int{17}, info: expectedInfo, exists: true}
	api.createFn = func() { t.Fatal("undurable intent allowed native creation") }
	var out bytes.Buffer
	if err := runWithConfig(createArgs(record), api, &out, os.Remove, config); err == nil || api.created || out.Len() != 0 {
		t.Fatalf("intent persistence failure reached native create: error=%v created=%t", err, api.created)
	}
}
