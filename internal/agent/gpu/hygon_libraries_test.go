package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
)

func TestHygonProbeLibrariesAllowsHostWithoutOptionalTinfo(t *testing.T) {
	// Preparing mounts may succeed without tinfo. The actual HIP probe still
	// rejects a DTK runtime whose dependencies cannot be loaded.
	listing := "libz.so.1 (libc6,x86-64) => /host/libz.so.1\n" +
		"libelf.so.1 (libc6,x86-64) => /host/libelf.so.1\n"
	got, err := hygonProbeLibraries(t.Context(), func(_ context.Context, name string) (string, error) {
		if path := parseLdconfigLibraryPath(listing, name); path != "" {
			return path, nil
		}
		return "", fmt.Errorf("probe dependency %s: %w", name, os.ErrNotExist)
	})
	want := []ProbeLibrary{{Source: "/host/libz.so.1", Name: "libz.so.1"}, {Source: "/host/libelf.so.1", Name: "libelf.so.1"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("probe libraries = %+v, %v; want %+v", got, err, want)
	}
}

func TestHygonProbeLibrariesMapsInstalledTinfoSONAMEs(t *testing.T) {
	for _, names := range [][]string{{"libtinfo.so.5"}, {"libtinfo.so.6"}, {"libtinfo.so.5", "libtinfo.so.6"}} {
		t.Run(fmt.Sprint(names), func(t *testing.T) {
			libraries := map[string]string{"libz.so.1": "/host/libz.so.1", "libelf.so.1": "/host/libelf.so.1"}
			want := []ProbeLibrary{{Source: "/host/libz.so.1", Name: "libz.so.1"}}
			for _, name := range names {
				libraries[name] = "/host/" + name
				want = append(want, ProbeLibrary{Source: libraries[name], Name: name})
			}
			want = append(want, ProbeLibrary{Source: "/host/libelf.so.1", Name: "libelf.so.1"})
			got, err := hygonProbeLibraries(t.Context(), func(_ context.Context, name string) (string, error) {
				if path, ok := libraries[name]; ok {
					return path, nil
				}
				return "", fmt.Errorf("probe dependency %s: %w", name, os.ErrNotExist)
			})
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("SONAME mounts = %+v, %v; want %+v", got, err, want)
			}
		})
	}
}

func TestHygonProbeLibrariesPreservesRequiredAndLookupFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"libz.so.1", os.ErrNotExist},
		{"libelf.so.1", os.ErrNotExist},
		{"libtinfo.so.5", os.ErrPermission},
		{"libtinfo.so.5", context.Canceled},
		{"libtinfo.so.6", os.ErrPermission},
		{"libtinfo.so.6", context.Canceled},
	} {
		t.Run(tc.name+"/"+tc.err.Error(), func(t *testing.T) {
			_, err := hygonProbeLibraries(t.Context(), func(_ context.Context, name string) (string, error) {
				if name == tc.name {
					return "", fmt.Errorf("lookup %s: %w", name, tc.err)
				}
				return "/host/" + name, nil
			})
			if !errors.Is(err, tc.err) {
				t.Fatalf("lookup failure = %v, want %v", err, tc.err)
			}
		})
	}
}

func TestHygonProbeLibrariesPreservesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := hygonProbeLibraries(ctx, func(_ context.Context, name string) (string, error) {
		if name == "libtinfo.so.5" {
			cancel()
			return "", os.ErrNotExist
		}
		return "/host/" + name, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled optional lookup = %v", err)
	}
	_, err = hygonProbeLibraries(ctx, func(context.Context, string) (string, error) {
		t.Fatal("canceled library lookup ran")
		return "", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup = %v", err)
	}
}
