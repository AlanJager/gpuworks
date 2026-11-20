package cluster

// The inventory is deliberately fixed to the complete current migrated schema.
// A migration must update this list with its behavior test; unknown tables or
// columns cannot silently disappear from a cold-maintenance checkpoint.
var maintenanceCheckpointColumns = map[string][]string{
	"action_drain_barriers":              {"cluster_id", "through_generation", "confirmed_at"},
	"action_grant_reservations":          {"grant_id", "cluster_id", "scheduling_generation", "node_uuid", "action_family", "conflict_resource_id", "owner_member_id", "session_generation", "session_uuid", "connection_id", "roster_revision", "not_after_unix_ms", "claims_sha256", "created_at"},
	"action_grant_roster_member_records": {"cluster_id", "revision", "member_id", "role", "key_id", "public_key", "revoked"},
	"action_grant_roster_records":        {"cluster_id", "revision", "not_after_unix_ms", "created_at"},
	"action_roster_drain_barriers":       {"cluster_id", "through_revision", "confirmed_at"},
	"agent_session_owners":               {"token", "session_uuid", "connection_id", "generation", "member_id"},
	"ai_configs":                         {"id", "name", "preset", "base_url", "api_key", "model", "is_default", "created_at", "updated_at"},
	"api_keys":                           {"id", "name", "prefix", "hash", "user_id", "created_at", "last_used"},
	"benchmark_executions":               {"id", "claim", "sequence", "owner_heartbeat_seq"},
	"benchmark_points":                   {"id", "run_id", "seq", "concurrency", "target_isl", "target_osl", "repeat", "state", "actual_isl", "actual_osl", "input_tokens", "output_tokens", "success", "errors", "output_tput", "total_tput", "max_output_tput", "max_total_tput", "request_tput", "avg_ttft", "p50_ttft", "p99_ttft", "avg_tpot", "p50_tpot", "p99_tpot", "avg_latency", "p50_latency", "p99_latency", "long_tail", "curve", "detail_path", "started_at", "ended_at", "created_at", "updated_at"},
	"benchmark_requests":                 {"owner_id", "request_key", "digest", "run_id"},
	"benchmark_runs":                     {"id", "name", "target_kind", "deployment_id", "instance_uuid", "base_url", "api_key", "model", "execution_claim", "load_origin", "num_gp_us", "dataset", "mode", "search", "tokenizer_ref", "target_snapshot", "warnings", "keep_detail", "detail_cap_mb", "state", "progress", "verdict", "error", "created_by", "started_at", "ended_at", "cancel_ack_at", "created_at", "updated_at"},
	"catalog_gc_barriers":                {"id", "sequence"},
	"client_request_claims":              {"owner_id", "request_key", "operation"},
	"cluster_admissions":                 {"key_fingerprint", "invitation_hash", "address", "admitted_at", "revoked_at", "material_grant_sequence", "enrollment_origin"},
	"cluster_authorizations":             {"id", "revision"},
	"cluster_identities":                 {"id", "cluster_id", "ca_fingerprint", "protocol", "created_at"},
	"cluster_invitations":                {"token_hash", "cluster_id", "issued_by", "created_at", "expires_at", "consumed_at", "authorized_enrollment_origin"},
	"committed_notifications":            {"request_key", "resource_id", "topic", "reason", "published", "revision"},
	"deployments":                        {"id", "name", "model_id", "template_id", "template_spec_source", "template_spec_name", "template_scenario", "template_deviated", "template_snapshot", "node_id", "node_ids", "served_name", "engine", "image", "quick_params", "extra_args", "env", "run_spec_overrides", "replicas", "v_ram_limit_bytes", "partition_backend", "sm_limit_pct", "sleepable", "desired_sleep", "placement", "dist_group", "dist_role", "n_nodes", "node_rank", "master_addr", "master_port", "restart_count", "desired_state", "created_at", "updated_at"},
	"gpus":                               {"id", "node_id", "index", "uuid", "name", "compute_capability", "arch_family", "vendor", "arch_id", "memory_total", "power_limit_watts", "pc_ie_bdf", "numa_node", "mig_enabled", "healthy", "health_status", "health_code", "health_message", "created_at", "updated_at"},
	"image_action_gates":                 {"image_version_id", "node_id", "image_node_id", "image_ref", "method", "transfer_id", "payload_sha256", "closed_generation", "prior_grant_not_after_unix_ms", "ready", "closed_at"},
	"image_import_requests":              {"owner_id", "request_key", "digest", "images_json", "response_json"},
	"image_nodes":                        {"id", "image_version_id", "node_id", "state", "size_bytes", "transferred_bytes", "rate_bps", "transfer_id", "source_node_id", "source", "message", "image_digest_id", "created_at", "updated_at"},
	"image_remove_intents":               {"id", "image_version_id", "node_id", "image_ref", "presence_updated_at", "confirmed", "created_at"},
	"image_series":                       {"id", "name", "engine", "purpose", "framework", "common_args", "common_env", "vendors", "source", "deprecated_since", "notes", "enabled", "created_at", "updated_at"},
	"image_versions":                     {"id", "series_id", "name", "engine", "engine_version", "framework", "framework_version", "variant", "arch", "image_ref", "source", "vendors", "env", "docker_args", "run_command", "deprecated", "enabled", "size_bytes", "verification_state", "verification_error", "notes", "offline_blob_path", "offline_blob_sha256", "offline_blob_bytes", "model_mount_path", "extra_mounts", "entrypoint_display", "cmd_display", "inspect_synced", "adapted_models", "deprecated_since", "registry_sources", "command_mode", "supported_gp_us", "unsupported_gp_us", "rolling_tag", "upstream_gone", "created_at", "updated_at"},
	"inference_models":                   {"id", "model_id", "source", "display_title", "parameter_count", "architecture", "input_modalities", "model_config", "kv_bytes_per_token", "readme_zh_cn", "readme_en", "package_digest", "context_length", "release_date", "unsupported", "created_at", "updated_at"},
	"inference_specs":                    {"id", "model_id", "source", "name", "package_digest", "display_profile", "scenario", "engine", "image", "quantization", "weight_sources", "weight_bytes", "runtime_weight_bytes", "gpus", "args", "env", "pre_start_argv", "pre_start_script", "assets", "entrypoint", "placement", "api_contract", "components", "created_at", "updated_at"},
	"instance_events":                    {"id", "instance_id", "instance_uuid", "deployment_id", "kind", "code", "message", "data", "created_at"},
	"instance_removals":                  {"uuid", "node_id", "version", "confirmed", "ack_token", "ack_connection_id", "ack_generation", "ack_member_id"},
	"instances":                          {"id", "uuid", "deployment_id", "node_id", "state", "gp_uindexes", "ports", "api_port", "container_id", "image", "image_digest_id", "spec", "image_defaults_snapshot", "run_spec_overrides", "measured", "schedule_expl", "direct_addr", "message", "phase", "progress", "bytes_done", "bytes_total", "rate_bps", "sleep_level", "dist_group", "dist_role", "n_nodes", "node_rank", "headless", "dist_attempt", "created_at", "updated_at"},
	"legacy_jobs":                        {"id", "kind", "name", "state", "params", "spec", "result", "node_id", "message", "created_at", "updated_at"},
	"maintenance_phase":                  {"id", "phase", "revision", "updated_at"},
	"maintenance_resume_open":            {"id", "run_id", "install_dir", "binary_sha256", "serving_revision", "prior_phase", "prior_revision", "open_revision", "member1_id", "member1_key", "member2_id", "member2_key", "member3_id", "member3_key"},
	"material_manifest_records":          {"source_member_id", "cluster_id", "source_key", "digest", "bytes", "recorded_at"},
	"material_remote_receipts":           {"digest", "member_id", "bytes", "recorded_at"},
	"material_replicas":                  {"digest", "member_id", "path", "recorded_at"},
	"model_files":                        {"id", "model_id", "node_id", "path", "revision", "state", "size_bytes", "downloaded", "transferred_bytes", "rate_bps", "rate_limit", "message", "proxy", "endpoint", "network_explicit", "acquisition", "transfer_id", "transfer_source_node_id", "eviction_snapshot_json", "created_at", "updated_at"},
	"model_logo_gc":                      {"digest", "generation", "deleting_name", "deletion_token"},
	"model_logo_reservations":            {"token", "digest", "name", "owner", "generation", "expires_at"},
	"model_registration_requests":        {"owner_id", "request_key", "digest", "model_id", "response_json", "status_code"},
	"model_sync_actions":                 {"id", "transfer_id", "source_file_id", "target_file_id", "source_stamp", "source_owner_stamp", "target_owner_stamp", "listen_params_json", "send_params_json", "scheduling_generation", "prior_grant_not_after_unix_ms", "ready"},
	"models":                             {"id", "source", "source_id", "revision", "source_url", "artifact_subfolder", "name", "arch", "quant_label", "weight_bytes", "dtype", "meta", "image_id", "readme", "logo_path", "origin_created_at", "origin_updated_at", "context_length", "precision", "operations", "input_modalities", "created_at", "updated_at"},
	"node_create_requests":               {"owner_id", "request_key", "digest", "node_uuid", "node_snapshot", "server_url_source"},
	"node_runtime_intents":               {"node_id", "version", "preference", "previous_preference", "payload_sha256", "state", "ready", "closed_generation", "prior_grant_not_after_unix_ms", "created_at", "updated_at"},
	"node_ssh_credential":                {"node_id", "host", "port", "user", "creds_cipher", "created_at", "updated_at"},
	"nodes":                              {"id", "uuid", "name", "token", "token_hash", "agent_version", "state", "hostname", "labels", "os", "kernel", "arch", "cpu_model", "cpu_cores", "mem_total_kb", "disk_total_kb", "gpu_count", "gpu_vendor", "gpu_driver", "nvidia_driver", "framework", "framework_version", "models_root", "path_usage", "direct_address", "server_url", "connect_mode", "server_dial_address", "server_dial_listen", "last_remote_addr", "runtime_driver", "runtime_preference", "runtime_plugin", "runtimes", "partition_assets", "partition_probes", "last_reject_reason", "last_reject_at", "last_seen_at", "install_claim", "install_phase", "install_token_hash", "install_credential_hash", "install_spec_json", "recovery_phase", "recovery_attempts", "last_probe_at", "last_recovery_error", "created_at", "updated_at", "topo_raw", "topo_json"},
	"offline_blob_gc_claims":             {"digest", "path", "state", "generation", "created_at", "updated_at"},
	"offline_image_import_requests":      {"owner_id", "request_key", "digest", "images_json", "response_json"},
	"profiles":                           {"id", "fingerprint", "model_id", "node_id", "engine", "vendor", "arch_id", "args_hash", "measured", "created_at", "updated_at"},
	"provider_bootstrap_tokens":          {"id", "token_hash", "provider_id", "external_id", "node_id", "expires_at", "used_at", "created_at", "updated_at"},
	"provider_install_claims":            {"provider_id", "external_id", "claim", "state", "node_id"},
	"provider_instances":                 {"id", "provider_id", "external_id", "name", "gpu_model", "gpu_count", "power", "price_per_hr", "region", "public_ip", "ssh_port", "ssh_user", "ssh_connections", "kind", "node_id", "created_at", "updated_at"},
	"providers":                          {"id", "name", "kind", "creds_cipher", "state", "created_at", "updated_at"},
	"quick_model_metadata_refreshes":     {"model_id", "base_updated_at", "lease_token", "lease_until_unix", "next_attempt_unix", "completed_at", "attempts"},
	"schema_migrations":                  {"version", "name", "applied_at"},
	"settings":                           {"key", "value", "updated_at"},
	"sqlite_sequence":                    {"name", "seq"},
	"storages":                           {"id", "name", "type", "enabled", "builtin", "config", "created_at", "updated_at"},
	"target_versions":                    {"resource", "version"},
	"terminal_sessions":                  {"id", "user_id", "username", "node_uuid", "target", "ref", "client_ip", "owner_member_id", "owner_run_id", "started_at", "ended_at", "bytes_in", "bytes_out", "close_reason"},
	"users":                              {"id", "username", "password_hash", "role", "created_at", "updated_at"},
}

// MaintenanceCheckpointInventory returns a private copy of the fixed wire
// schema. Callers cannot mutate the validator's authoritative inventory.
func MaintenanceCheckpointInventory() map[string][]string {
	copy := make(map[string][]string, len(maintenanceCheckpointColumns))
	for name, columns := range maintenanceCheckpointColumns {
		copy[name] = append([]string(nil), columns...)
	}
	return copy
}
