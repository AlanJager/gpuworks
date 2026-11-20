"""Prepare a pinned private Go SDK without modifying the module cache."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile

MODULE = 'github.com/canonical/go-dqlite/v3'
VERSION = 'v3.0.4'
SOURCE_SHA256 = 'edda9735c7ae85e68b86d6fddc6847ba9c3c478c47da3e4fdfe635a58da46fa9'
SOURCE_TREE_SHA256 = 'd41bb3b011f3c5b380fa2047b22a415ef4f6d10674c64c332019734bfcfe617d'
PATCH_SHA256 = 'da3931c44466f9c97369e53c420b59fb72a10835608ec2536cad768b3c9e020c'
PATCHED_SHA256 = 'a8bdc45853f7d06ac26c0eb3c906ad1c393911fbe01a6a27c33340677bd7b75a'
NODE_STORE_SOURCE_SHA256 = '64a7970823fd9b75ddeb23ea8ff71c15c14fcab1bbbf9ad2eb8aef936329086e'
NODE_STORE_PATCH_SHA256 = 'efef3a2f3bae3689f348911c20cadd61ee680585232fc7f508b83ad00f3d64bf'
NODE_STORE_PATCHED_SHA256 = '33a15a88d1655fafc4bc8577ea4a614f0b268a36fb7b460d589851a7fd98fe26'
NODE_STORE_HELPER_SHA256 = '114168e64f2206fbb808f59e1ffff3136d2dd8cd1e27918bde7435e6bc8b3a22'
RPC_CANCEL_PATCH_SHA256 = 'bf02f3b86c681c096adf6367f6e33ba9c78caa5ad2cb7f3bfab9f327c65dc66b'
RPC_CANCEL_MODULE_SHA256 = 'b87c174a011083e9826ef63783804879abba7c16d71467cd74655fe489fa5730'
RPC_CANCEL_SOURCE_FILES = {'driver/driver.go': '542d4bfb16ad17ab3d8cf7704f461244a7bd2af523c5369fe279eaeada4f82e8',
 'internal/protocol/protocol.go': 'b7ca049b3118469fa9312cd52d40e065a3e5ed2335fd2a81d0fb57828195619b'}
RPC_CANCEL_PATCHED_FILES = {'driver/driver.go': '01ee27596a9767735dcfd616ebede48961411adde84087f048083d5d4e21e3ab',
 'internal/protocol/protocol.go': '509f8cd08564d2b39ada61b51a7f65286f27a3dd1661d43548925462c5af1795'}
PREVIOUS_HELPER_SHA256 = '5819f35581722d905f66e374a7599bbd5c7c32b2d87747d8ffa3172381bc25b5'
OLD_HELPER_SHA256 = '640b81f4829f25b2a4347bf05ebdc05532faae3109cd54cceb429fca8113019d'
ARTIFACTS = {'go-dqlite-unix-socket.patch', 'prepare_go_sdk.py', 'gpuworks-native.mod',
             'gpuworks-native.sum', 'env.sh'}
PREVIOUS_ARTIFACTS = set(ARTIFACTS)
ARTIFACTS = ARTIFACTS | {'go-dqlite-node-store.patch'}
NODE_STORE_ARTIFACTS = set(ARTIFACTS)
ARTIFACTS = ARTIFACTS | {'go-dqlite-rpc-cancel.patch'}
OLD_ARTIFACTS = {'app/app.go', 'go-dqlite-unix-socket.patch', 'prepare_go_sdk.py',
                 'go-compiler-overlay.json', 'env.sh'}


def digest(path):
    if path.is_symlink() or not path.is_file():
        raise ValueError('SDK artifact must be a regular file: ' + str(path))
    return hashlib.sha256(path.read_bytes()).hexdigest()


def file_hashes(root):
    if root.is_symlink() or not root.is_dir():
        raise ValueError('SDK directory must be regular: ' + str(root))
    result = {}
    for path in sorted(root.rglob('*')):
        if path.is_symlink() or not (path.is_file() or path.is_dir()):
            raise ValueError('SDK source contains a non-regular entry: ' + str(path))
        if path.is_file():
            result[str(path.relative_to(root))] = digest(path)
    return result


def tree_digest(files):
    return hashlib.sha256(json.dumps(files, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def go_output(go, args, cwd=None, flags=None):
    # Module and toolchain discovery must never fetch anything, even when the
    # caller has a network proxy or automatic toolchain selection configured.
    env = dict(os.environ, GOPROXY='off', GOSUMDB='off', GOTOOLCHAIN='local',
               GONOPROXY='none', GOVCS='*:off')
    if flags is not None:
        env['GOFLAGS'] = flags
    result = subprocess.run([go, *args], cwd=cwd, env=env, capture_output=True, text=True, timeout=30)
    if result.returncode:
        raise ValueError('offline Go SDK command failed: ' + result.stderr.strip()
                         + '; select a compatible locally installed toolchain with GO')
    return result.stdout.strip()


def flag_mode(flags, prefix, known=None, prepare=False, legacy=False):
    """Remove only verified own discovery flags; reject foreign overrides."""
    own_mod = '-modfile=' + str(prefix / 'go-sdk/gpuworks-native.mod')
    own_old = '-overlay=' + str(prefix / 'go-sdk/go-compiler-overlay.json')
    kept = []
    found_mod = False
    changed = False
    for token in shlex.split(flags):
        if token in ('-overlay', '--overlay', '-modfile', '--modfile') or token.startswith(
                ('-overlay=', '--overlay=', '-modfile=', '--modfile=')):
            normalized = token[1:] if token.startswith('--') else token
            if not ((normalized == own_mod and known == 'modfile') or
                    (normalized == own_old and ((known == 'overlay' and prepare) or
                                                (known == 'modfile' and legacy)))):
                raise ValueError('refusing unknown GOFLAGS overlay/modfile')
            if prepare or normalized == own_old:
                changed = True
            else:
                found_mod = True
                kept.append(token)
        else:
            kept.append(token)
    result = shlex.join(kept) if changed else flags
    if prepare or found_mod:
        return result
    return (result + ' ' if result else '') + shlex.quote(own_mod)


def sdk_env(target):
    return ('# Pinned private Go SDK; preserve unrelated flags and reject other overrides.\n'
            '_gpuworks_sdk_flags=$(python3 ' + shlex.quote(str(target / 'go-sdk/prepare_go_sdk.py'))
            + ' flags --prefix ' + shlex.quote(str(target)) + ' --go "${GO:-go}")'
            + ' || { return 1 2>/dev/null || exit 1; }\n'
            'export GOFLAGS="$_gpuworks_sdk_flags"\n'
            'unset _gpuworks_sdk_flags\n')


def verify(sdk, installed=None):
    installed = installed or sdk
    manifest = json.loads((sdk / 'manifest.json').read_text())
    if (manifest.get('module') != MODULE or manifest.get('version') != VERSION
            or manifest.get('source_sha256') != SOURCE_SHA256
            or manifest.get('patch_sha256') != PATCH_SHA256
            or manifest.get('patched_sha256') != PATCHED_SHA256):
        raise ValueError('unknown generated Go SDK identity')
    actual = file_hashes(sdk)
    actual.pop('manifest.json')
    if actual != manifest.get('files'):
        raise ValueError('generated Go SDK artifact hash mismatch')
    if set(actual) == OLD_ARTIFACTS:
        if set(manifest) != {'module', 'version', 'source_file', 'source_sha256', 'patch_sha256',
                             'patched_sha256', 'patched_file', 'files'}:
            raise ValueError('unknown old Go SDK overlay manifest')
        old_env = sdk_env(installed.parent).replace(
            '# Pinned private Go SDK; preserve unrelated flags and reject other overrides.',
            '# Pinned Go SDK patch; preserve unrelated flags and reject other overlays.')
        if (sdk / 'env.sh').read_text() != old_env:
            raise ValueError('unknown old Go SDK overlay environment')
        if (actual['prepare_go_sdk.py'] != OLD_HELPER_SHA256
                or actual['app/app.go'] != PATCHED_SHA256
                or actual['go-dqlite-unix-socket.patch'] != PATCH_SHA256):
            raise ValueError('unknown old Go SDK overlay identity')
        if manifest['patched_file'] != str(installed / 'app/app.go'):
            raise ValueError('old SDK overlay installed prefix differs')
        overlay = json.loads((sdk / 'go-compiler-overlay.json').read_text())
        if overlay != {'Replace': {manifest['source_file']: manifest['patched_file']}}:
            raise ValueError('unknown compiler overlay mapping')
        if digest(Path(manifest['source_file'])) != SOURCE_SHA256:
            raise ValueError('old SDK overlay source changed')
        return manifest, 'overlay'
    module_files = {name[7:]: value for name, value in actual.items() if name.startswith('module/')}
    node_store_patched = 'go-dqlite-node-store.patch' in actual
    rpc_cancel_patched = 'go-dqlite-rpc-cancel.patch' in actual
    artifacts = (ARTIFACTS if rpc_cancel_patched else
                 NODE_STORE_ARTIFACTS if node_store_patched else PREVIOUS_ARTIFACTS)
    if (set(actual) != artifacts | {'module/' + name for name in module_files}
            or module_files.get('app/app.go') != PATCHED_SHA256
            or actual.get('go-dqlite-unix-socket.patch') != PATCH_SHA256):
        raise ValueError('unknown private SDK artifacts')
    expected_manifest = {'module', 'version', 'source_sha256', 'source_files', 'patch_sha256',
                         'patched_sha256', 'root_files', 'modfile', 'upgraded_from_overlay', 'files'}
    if node_store_patched:
        expected_manifest |= {'node_store_patch_sha256', 'node_store_patched_sha256'}
    if rpc_cancel_patched:
        expected_manifest |= {'rpc_cancel_patch_sha256', 'rpc_cancel_module_sha256', 'rpc_cancel_patched_files'}
    if set(manifest) != expected_manifest:
        raise ValueError('unknown private SDK manifest')
    if (sdk / 'env.sh').read_text() != sdk_env(installed.parent):
        raise ValueError('unknown private SDK environment')
    if node_store_patched:
        if (actual['go-dqlite-node-store.patch'] != NODE_STORE_PATCH_SHA256
                or module_files.get('client/store.go') != NODE_STORE_PATCHED_SHA256
                or manifest.get('node_store_patch_sha256') != NODE_STORE_PATCH_SHA256
                or manifest.get('node_store_patched_sha256') != NODE_STORE_PATCHED_SHA256):
            raise ValueError('unknown node store SDK identity')
    elif actual.get('prepare_go_sdk.py') != PREVIOUS_HELPER_SHA256:
        raise ValueError('unknown previous private SDK helper')
    if rpc_cancel_patched:
        if (not node_store_patched
                or actual['go-dqlite-rpc-cancel.patch'] != RPC_CANCEL_PATCH_SHA256
                or manifest.get('rpc_cancel_patch_sha256') != RPC_CANCEL_PATCH_SHA256
                or manifest.get('rpc_cancel_module_sha256') != RPC_CANCEL_MODULE_SHA256
                or manifest.get('rpc_cancel_patched_files') != RPC_CANCEL_PATCHED_FILES
                or any(module_files.get(name) != value for name, value in RPC_CANCEL_PATCHED_FILES.items())
                or tree_digest(module_files) != RPC_CANCEL_MODULE_SHA256):
            raise ValueError('unknown RPC cancellation SDK identity')
        if actual['prepare_go_sdk.py'] != digest(Path(__file__).resolve()):
            raise ValueError('unknown RPC cancellation SDK helper')
    elif node_store_patched and actual['prepare_go_sdk.py'] != NODE_STORE_HELPER_SHA256:
        raise ValueError('unknown node store SDK helper')
    original_files = dict(module_files, **{'app/app.go': SOURCE_SHA256})
    if rpc_cancel_patched:
        for name in RPC_CANCEL_PATCHED_FILES:
            if name in RPC_CANCEL_SOURCE_FILES:
                original_files[name] = RPC_CANCEL_SOURCE_FILES[name]
            else:
                original_files.pop(name, None)
    if node_store_patched:
        original_files['client/store.go'] = NODE_STORE_SOURCE_SHA256
    if (manifest.get('source_files') != original_files
            or tree_digest(original_files) != SOURCE_TREE_SHA256):
        raise ValueError('private SDK differs from the complete pinned source')
    if type(manifest.get('upgraded_from_overlay')) is not bool:
        raise ValueError('unknown SDK migration identity')
    if manifest.get('modfile') != str(installed / 'gpuworks-native.mod'):
        raise ValueError('private SDK installed prefix differs')
    return manifest, 'modfile'


def prepare(prefix, target, module, patch, repo, go, flags='', legacy=False):
    if module.get('Path') != MODULE or module.get('Version') != VERSION or 'Replace' in module:
        raise ValueError('native recipe requires the exact unmodified Go SDK ' + MODULE + '@' + VERSION)
    if not isinstance(module.get('Dir'), str) or not module['Dir']:
        raise ValueError('pinned Go SDK must already be present in the module cache')
    source = Path(module['Dir'])
    source_files = file_hashes(source)
    if not source.is_absolute() or tree_digest(source_files) != SOURCE_TREE_SHA256:
        raise ValueError('pinned Go SDK source hash mismatch')
    if digest(patch) != PATCH_SHA256:
        raise ValueError('pinned Go SDK patch hash mismatch')
    node_patch = Path(__file__).with_name('go-dqlite-node-store.patch')
    if digest(node_patch) != NODE_STORE_PATCH_SHA256:
        raise ValueError('pinned node store patch hash mismatch')
    rpc_patch = Path(__file__).with_name('go-dqlite-rpc-cancel.patch')
    if digest(rpc_patch) != RPC_CANCEL_PATCH_SHA256:
        raise ValueError('pinned RPC cancellation patch hash mismatch')
    sdk = prefix / 'go-sdk'
    previous, mode = verify(sdk, target / 'go-sdk') if sdk.exists() or sdk.is_symlink() else (None, None)
    legacy = legacy or mode == 'overlay' or bool(previous and previous.get('upgraded_from_overlay'))
    root_files = {str(repo / name): digest(repo / name) for name in ('go.mod', 'go.sum')}
    with tempfile.TemporaryDirectory(prefix='.go-sdk.', dir=prefix) as temporary:
        staging = Path(temporary)
        staged = staging / 'next'
        staged.mkdir()
        shutil.copytree(source, staged / 'module')
        for path in (staged / 'module').rglob('*'):
            path.chmod(0o700 if path.is_dir() else 0o600)
        (staged / 'module').chmod(0o700)
        shutil.copyfile(patch, staged / patch.name)
        result = subprocess.run(['patch', '--batch', '--forward', '--fuzz=0', '-p1',
                                 '-d', str(staged / 'module'), '-i', str(patch)],
                                capture_output=True, text=True, timeout=30)
        if result.returncode or digest(staged / 'module/app/app.go') != PATCHED_SHA256:
            raise ValueError('strict Go SDK patch failed: ' + result.stdout.strip() + ' ' + result.stderr.strip())
        shutil.copyfile(node_patch, staged / node_patch.name)
        result = subprocess.run(['patch', '--batch', '--forward', '--fuzz=0', '-p1',
                                 '-d', str(staged / 'module'), '-i', str(node_patch)],
                                capture_output=True, text=True, timeout=30)
        if result.returncode or digest(staged / 'module/client/store.go') != NODE_STORE_PATCHED_SHA256:
            raise ValueError('strict node store patch failed: ' + result.stdout.strip() + ' ' + result.stderr.strip())
        shutil.copyfile(rpc_patch, staged / rpc_patch.name)
        result = subprocess.run(['patch', '--batch', '--forward', '--fuzz=0', '-p1',
                                 '-d', str(staged / 'module'), '-i', str(rpc_patch)],
                                capture_output=True, text=True, timeout=30)
        if (result.returncode
                or any(digest(staged / 'module' / name) != value for name, value in RPC_CANCEL_PATCHED_FILES.items())
                or tree_digest(file_hashes(staged / 'module')) != RPC_CANCEL_MODULE_SHA256):
            raise ValueError('strict RPC cancellation patch failed: ' + result.stdout.strip() + ' ' + result.stderr.strip())
        if file_hashes(source) != source_files:
            raise ValueError('Go SDK source changed during preparation')
        shutil.copyfile(Path(__file__).resolve(), staged / 'prepare_go_sdk.py')
        shutil.copyfile(repo / 'go.mod', staged / 'gpuworks-native.mod')
        shutil.copyfile(repo / 'go.sum', staged / 'gpuworks-native.sum')
        go_output(go, ['mod', 'edit', '-modfile=' + str(staged / 'gpuworks-native.mod'),
                      '-replace=' + MODULE + '=' + str(target / 'go-sdk/module')], repo, flags)
        (staged / 'env.sh').write_text(sdk_env(target))
        manifest = {'module': MODULE, 'version': VERSION, 'source_sha256': SOURCE_SHA256,
                    'source_files': source_files, 'patch_sha256': PATCH_SHA256,
                    'patched_sha256': PATCHED_SHA256, 'root_files': root_files,
                    'node_store_patch_sha256': NODE_STORE_PATCH_SHA256,
                    'node_store_patched_sha256': NODE_STORE_PATCHED_SHA256,
                    'rpc_cancel_patch_sha256': RPC_CANCEL_PATCH_SHA256,
                    'rpc_cancel_module_sha256': RPC_CANCEL_MODULE_SHA256,
                    'rpc_cancel_patched_files': RPC_CANCEL_PATCHED_FILES,
                    'modfile': str(target / 'go-sdk/gpuworks-native.mod'),
                    'upgraded_from_overlay': legacy,
                    'files': file_hashes(staged)}
        (staged / 'manifest.json').write_text(json.dumps(manifest, indent=2, sort_keys=True) + '\n')
        verify(staged, target / 'go-sdk')
        if any(digest(Path(name)) != value for name, value in root_files.items()):
            raise ValueError('repository module files changed during preparation')
        if sdk.exists():
            sdk.rename(staging / 'previous')
        try:
            staged.rename(sdk)
        except Exception:
            if (staging / 'previous').exists():
                (staging / 'previous').rename(sdk)
            raise
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('prepare', 'flags'))
    parser.add_argument('--prefix', type=Path, required=True)
    parser.add_argument('--target-prefix', type=Path)
    parser.add_argument('--repo', type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument('--go', default=os.environ.get('GO', 'go'),
                        help='compatible locally installed Go; downloads are disabled')
    args = parser.parse_args()
    prefix = args.prefix.absolute()
    target = (args.target_prefix or prefix).absolute()
    if prefix.is_symlink() or not (prefix / '.gpuworks-dqlite').is_file():
        raise ValueError('Go SDK preparation requires a managed native prefix')
    with (prefix / '.go-sdk.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        existing = prefix / 'go-sdk'
        manifest, mode = verify(existing, target / 'go-sdk') if existing.exists() or existing.is_symlink() else (None, None)
        current = go_output(args.go, ['env', 'GOFLAGS'])
        if args.action == 'flags':
            if mode != 'modfile':
                raise ValueError('private SDK modfile is not prepared')
            if any(digest(Path(name)) != value for name, value in manifest['root_files'].items()):
                raise ValueError('repository module files differ; regenerate the native SDK')
            print(flag_mode(current, prefix, mode, legacy=manifest['upgraded_from_overlay']))
            return
        flag_prefix, flag_manifest, flag_kind = prefix, manifest, mode
        if target != prefix:
            target_sdk = target / 'go-sdk'
            flag_prefix, flag_manifest, flag_kind = target, None, None
            if target_sdk.exists() or target_sdk.is_symlink():
                if target.is_symlink() or not (target / '.gpuworks-dqlite').is_file():
                    raise ValueError('compiler flag target requires a managed native prefix')
                flag_manifest, flag_kind = verify(target_sdk)
        legacy = flag_kind == 'overlay' or bool(flag_manifest and flag_manifest.get('upgraded_from_overlay'))
        flags = flag_mode(current, flag_prefix, flag_kind, prepare=True, legacy=legacy)
        module = json.loads(go_output(args.go, ['list', '-m', '-json', MODULE], args.repo, flags))
        manifest = prepare(prefix, target, module, Path(__file__).with_name('go-dqlite-unix-socket.patch').resolve(),
                           args.repo.absolute(), args.go, flags, legacy=legacy)
    print(json.dumps({key: manifest[key] for key in ('module', 'version', 'modfile', 'source_sha256', 'patch_sha256', 'patched_sha256')}))


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, subprocess.SubprocessError, KeyError) as error:
        print('dqlite Go SDK: ' + str(error), file=sys.stderr)
        raise SystemExit(1)
