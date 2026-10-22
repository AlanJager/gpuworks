// Package registryprobe orders the registries a node may pull an image from by
// how well this node reaches them, and remembers which ones just failed.
//
// It is a leaf: it knows reference syntax and HTTP, nothing about runtimes.
package registryprobe

import "strings"

// DockerHub is the name every Docker Hub spelling normalizes to.
const DockerHub = "docker.io"

// NormalizeHost lowercases a registry host and folds Docker Hub's spellings
// (docker.io, index.docker.io, registry-1.docker.io) into DockerHub, so a
// credential, a cooldown and a probe result for one registry are shared by
// every reference to it.
func NormalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimRight(host, "/")
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return DockerHub
	}
	return host
}

// HostOf is the registry host an image reference is pulled from. Like
// Docker, it reads the first path component as a host only when it holds a
// dot or a colon or is "localhost"; any other reference lives on Docker Hub.
func HostOf(ref string) string {
	ref = strings.TrimSpace(ref)
	first, _, hasPath := strings.Cut(ref, "/")
	if !hasPath {
		return DockerHub
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return NormalizeHost(first)
	}
	return DockerHub
}

// probeHost is the host a /v2/ probe is sent to: Docker Hub's API does not
// answer on docker.io itself.
func probeHost(host string) string {
	if host == DockerHub {
		return "registry-1.docker.io"
	}
	return host
}
