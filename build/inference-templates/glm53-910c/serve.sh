#!/usr/bin/env bash
set -eo pipefail
case "$1:$2:$3" in
  1:16:2|1:8:4) ;;
  *) echo 'GLM-5.3-Flash requires one node: 16 logical NPUs with TP2, or 8 with TP4.' >&2; exit 2 ;;
esac
shift 3
bundle=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
python "$bundle/install.py"
# Match the pinned vendor image's entrypoint environment.
source /usr/local/Ascend/ascend-toolkit/set_env.sh
source "/usr/local/Ascend/cann-${CANN_VERSION}/share/info/ascendnpu-ir/bin/set_env.sh"
source /usr/local/Ascend/nnal/atb/set_env.sh --cxx_abi="${ATB_CXX_ABI:-1}"
exec vllm "$@"
