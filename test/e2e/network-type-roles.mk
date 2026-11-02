.PHONY: test-e2e-network-type-roles
test-e2e-network-type-roles: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/network_type_roles.py --locale en
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/network_type_roles.py --locale zh
