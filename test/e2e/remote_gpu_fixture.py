"""Owned real GPU deployment and cookie-free public inference transport.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Deployment creation is fixture setup, never a browser deployment coverage claim.
"""
import json
import urllib.error
import urllib.request


def prepare_gpu(r):
    index = int(r.config['gpu_index'])
    image = r.config['image']
    baseline = r.host(f'''
import subprocess
containers=subprocess.check_output(['docker','ps','-q','--no-trunc'],text=True).split()
gpu=subprocess.check_output(['nvidia-smi','-i',{str(index)!r},'--query-gpu=uuid,memory.used','--format=csv,noheader,nounits'],text=True).strip().split(', ')
apps=subprocess.check_output(['nvidia-smi','--query-compute-apps=gpu_uuid,pid','--format=csv,noheader,nounits'],text=True).splitlines()
assert not any(line.split(',')[0].strip()==gpu[0] for line in apps), 'BLOCKED_INFRA: selected GPU has compute processes'
image=json.loads(subprocess.check_output(['docker','image','inspect',{image!r}],text=True))[0]
result={{'containers':containers,'gpu_uuid':gpu[0],'memory_mib':int(gpu[1]),'image_id':image['Id'],'image_digests':image['RepoDigests']}}
''')
    assert baseline['memory_mib'] < 100, 'BLOCKED_INFRA: selected GPU is occupied'
    r.gpu_baseline = baseline
    model = r.api('GET', '/api/v1/models/' + str(r.config['model_id']))
    assert model['source_id'] == r.config['model_source_id'] and model['revision'] == r.config['model_revision'], 'explicit model identity changed'
    name = 'e2e-' + r.run_id + '-gpu'
    body = {'name': name, 'served_name': name, 'model_id': model['id'],
        'engine': 'vllm', 'image': image, 'node_uuid': r.node['uuid'],
        'placement_mode': 'manual', 'placement': {'tp': 1, 'gpus': [index]},
        'model_source': {'kind': 'local_present', 'storage_id': r.config['storage_source_id']},
        'image_pull': {'kind': 'local_present', 'image': image},
        'quick_params': {'max_model_len': 2048, 'gpu_memory_utilization': 0.15, 'enforce_eager': True},
        'replicas': 1, 'start_timeout_sec': 900}
    r.evidence('fixture', {'baseline': baseline, 'model': {k: model.get(k) for k in ('id', 'source_id', 'revision', 'nodes', 'storages')}, 'deployment_request': body})
    created = r.api('POST', '/api/v1/deployments', body, expected=201)
    r.own('deployment', created['id'])
    r.deployment = created
    path = '/api/v1/deployments/' + str(created['id'])
    last_state = None
    def read():
        nonlocal last_state
        value = r.api('GET', path)
        states = [(i['uuid'], i['state'], i.get('message')) for i in value.get('instances', [])]
        if states != last_state:
            r.artifacts.write_json('deployment-progress.json', value)
            print('Deployment:', [(s[0], s[1]) for s in states], flush=True)
            last_state = states
        assert not any(s[1] == 'failed' for s in states), 'real deployment failed; inspect deployment-progress.json'
        return value
    r.deployment = r.poll(read, lambda d: len(d.get('instances', [])) == 1 and d['instances'][0]['state'] == 'running', timeout=900)
    instance = r.deployment['instances'][0]
    assert instance['node_uuid'] == r.node['uuid'] and instance['gpu_indexes'] == [index]
    r.container_id = instance['container_id']
    assert r.container_id and all(not c.startswith(r.container_id) for c in baseline['containers'])
    r.model_baseline = model
    verify_gpu(r)


def verify_gpu(r):
    evidence = r.host(f'''
import subprocess
cid={r.container_id!r}
c=json.loads(subprocess.check_output(['docker','inspect',cid],text=True))[0]
top=subprocess.check_output(['docker','top',cid,'-eo','pid'],text=True).splitlines()[1:]
pids={{line.strip() for line in top}}
apps=[]
for line in subprocess.check_output(['nvidia-smi','--query-compute-apps=gpu_uuid,pid,used_gpu_memory','--format=csv,noheader,nounits'],text=True).splitlines():
    gpu,pid,memory=[x.strip() for x in line.split(',')]
    if gpu=={r.gpu_baseline['gpu_uuid']!r} and pid in pids:
        apps.append({{'gpu_uuid':gpu,'pid':int(pid),'memory_mib':int(memory)}})
assert apps and all(a['memory_mib']>0 for a in apps), 'no real container GPU process'
assert c['State']['Running'] and c['Image']=={r.gpu_baseline['image_id']!r}
result={{'container_id':c['Id'],'image_id':c['Image'],'command':c['Config']['Cmd'],'gpu_processes':apps,
'mounts':[{{'source':m['Source'],'destination':m['Destination'],'rw':m['RW']}} for m in c['Mounts']],
'device_requests':c['HostConfig']['DeviceRequests']}}
''')
    r.evidence('execution', {'deployment_id': r.deployment['id'], 'instance': r.deployment['instances'][0], **evidence})


def public_request(r, key, path, body=None):
    headers = {'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json'}
    request = urllib.request.Request(r.backend_base + path, headers=headers,
        data=None if body is None else json.dumps(body).encode())
    # A new transport without admin cookies; never follow an auth-bearing redirect.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            return None
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        response = opener.open(request, timeout=120)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, response.headers.get('Content-Type', ''), response.read().decode()


def public_chat(r, key, stream=False):
    body = {'model': r.deployment['served_name'], 'messages': [{'role': 'user', 'content': 'Reply with one short sentence about the sky.'}],
        'max_tokens': 64, 'temperature': 0, 'stream': stream, 'chat_template_kwargs': {'enable_thinking': False}}
    if stream:
        body['stream_options'] = {'include_usage': True}
    status, content_type, raw = public_request(r, key, '/v1/chat/completions', body)
    assert status == 200, f'chat HTTP {status}: {r.artifacts.redact(raw)[:1000]}'
    if stream:
        assert 'text/event-stream' in content_type
        frames = [line[5:].strip() for line in raw.splitlines() if line.startswith('data:')]
        assert frames and frames[-1] == '[DONE]'
        chunks = [json.loads(frame) for frame in frames[:-1]]
        assert chunks and len({c['id'] for c in chunks}) == 1
        assert all(c['model'] == body['model'] for c in chunks)
        content = ''.join(choice.get('delta', {}).get('content', '') or '' for c in chunks for choice in c.get('choices', []))
        finishes = [choice['finish_reason'] for c in chunks for choice in c.get('choices', []) if choice.get('finish_reason')]
        usage = [c['usage'] for c in chunks if c.get('usage')][-1]
        result = {'id': chunks[0]['id'], 'content': content, 'usage': usage, 'finish_reason': finishes[-1], 'chunks': len(chunks)}
    else:
        value = json.loads(raw)
        assert value['object'] == 'chat.completion' and value['model'] == body['model']
        choice = value['choices'][0]
        assert choice['message']['role'] == 'assistant'
        result = {'id': value['id'], 'content': choice['message']['content'], 'usage': value['usage'], 'finish_reason': choice['finish_reason']}
    assert result['id'] and result['content'].strip() and result['finish_reason'] == 'stop'
    usage = result['usage']
    assert usage['prompt_tokens'] > 0 and usage['completion_tokens'] > 0
    assert usage['total_tokens'] == usage['prompt_tokens'] + usage['completion_tokens']
    r.evidence('runtime', {'method': 'POST', 'path': '/v1/chat/completions', 'body': body, 'status': status, 'content_type': content_type, 'response': result})
    return result


def cleanup_gpu(r):
    deployment_id = r.deployment['id']
    deleted = r.api('DELETE', '/api/v1/deployments/' + str(deployment_id), expected=202)
    assert deleted == {'status': 'deleting'}
    r.poll(lambda: r.api('GET', '/api/v1/instances')['items'], lambda items: all(i['deployment_id'] != deployment_id for i in items))
    def host_state():
        return r.host(f'''
import subprocess
containers=subprocess.check_output(['docker','ps','-aq','--no-trunc'],text=True).split()
active=subprocess.check_output(['docker','ps','-q','--no-trunc'],text=True).split()
memory=int(subprocess.check_output(['nvidia-smi','-i',{str(r.config['gpu_index'])!r},'--query-gpu=memory.used','--format=csv,noheader,nounits'],text=True).strip())
result={{'containers':containers,'active':active,'memory_mib':memory}}
''')
    state = r.poll(host_state, lambda s: not any(c.startswith(r.container_id) for c in s['containers']) and s['memory_mib'] < 100, timeout=120)
    assert set(r.gpu_baseline['containers']) <= set(state['active']), 'baseline container stopped'
    def absent():
        response = r.context.request.get(r.base + '/api/v1/deployments/' + str(deployment_id))
        assert response.status in (200, 404), 'unexpected deployment deletion status'
        if response.status == 200:
            assert response.json()['id'] == deployment_id and response.json()['desired_state'] == 'deleting'
        return response.status == 404
    r.poll(absent, bool)
    r.forget('deployment', deployment_id)
    model_after = r.api('GET', '/api/v1/models/' + str(r.config['model_id']))
    r.evidence('fixture', {'shared_model_id': r.config['model_id'],
        'node_cache_before': r.model_baseline.get('nodes', []),
        'node_cache_after': model_after.get('nodes', []),
        'cache_policy': 'Shared model/cache belongs to the reusable fixture; never evict it as run-owned data'})
    r.evidence('cleanup', {'deployment_id': deployment_id, 'container_removed': True, 'gpu_memory_mib': state['memory_mib'], 'baseline_containers_running': True})
