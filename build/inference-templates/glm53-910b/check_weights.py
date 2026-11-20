#!/usr/bin/env python3
"""Check the installed real weight loader using tiny CPU tensors, before TP startup."""


def check_weights():
    import torch
    from torch import nn
    from vllm_ascend.models.glm5next.model import Glm5NextForCausalLM

    model = Glm5NextForCausalLM.__new__(Glm5NextForCausalLM)
    nn.Module.__init__(model)
    model.model = nn.Module()
    layer = nn.Module()
    layer.self_attn = nn.Module()
    layer.self_attn.in_proj_qkvbfg_a = nn.Linear(2, 2, bias=False, device='cpu')
    layer.self_attn.A_log = nn.Parameter(torch.zeros(2, device='cpu'))
    layer.hc_attn_fn = nn.Parameter(torch.zeros(2, device='cpu'))
    layer.hc_ffn_scale = nn.Parameter(torch.zeros(2, device='cpu'))
    model.model.layers = nn.ModuleList([layer])
    model.lm_head = nn.Linear(2, 2, bias=False, device='cpu')

    checkpoint_names = {
        'model.language_model.layers.0.self_attn.in_proj_qkvbfg_a.weight': 'model.layers.0.self_attn.in_proj_qkvbfg_a.weight',
        'model.language_model.layers.0.self_attn.forget_gate.A_log': 'model.layers.0.self_attn.A_log',
        'model.language_model.layers.0.attn_hc.fn': 'model.layers.0.hc_attn_fn',
        'model.language_model.layers.0.ffn_hc.scale': 'model.layers.0.hc_ffn_scale',
        'lm_head.weight': 'lm_head.weight',
    }
    parameters = dict(model.named_parameters())
    weights = [(source, torch.full_like(parameters[target], index + 1))
               for index, (source, target) in enumerate(checkpoint_names.items())]
    expected = {target: tensor.clone()
                for (_, target), (_, tensor) in zip(checkpoint_names.items(), weights)}
    weights.append(('model.visual.blocks.0.norm1.weight', torch.ones(2, device='cpu')))
    weights.append(('rot.weight', torch.ones((2, 2), device='cpu')))
    loaded = model.load_weights(iter(weights))
    if loaded != set(expected):
        raise AssertionError(f'weight loader omitted parameters: {set(expected) - loaded}')
    for name, value in expected.items():
        torch.testing.assert_close(parameters[name], value)

    # Already canonical checkpoint names must continue to load unchanged.
    canonical = [(name, torch.full_like(value, 9)) for name, value in expected.items()]
    model.load_weights(iter(canonical))
    for name, _ in canonical:
        torch.testing.assert_close(parameters[name], torch.full_like(parameters[name], 9))
    try:
        model.load_weights(iter([('model.unexpected.weight', torch.ones(2, device='cpu'))]))
    except (KeyError, ValueError):
        pass
    else:
        raise AssertionError('unrecognized text weights must not be silently skipped')
    print('GLM_WEIGHT_MAPPING_OK prefix KDA mHC canonical skipped_visual skipped_visual_rotation strict_text', flush=True)


if __name__ == '__main__':
    check_weights()
    from check_draft_config import check_draft_config
    check_draft_config()
