class FakePipeline:
    def __init__(self, task):
        self.task = task

    def __call__(self, value, **_kwargs):
        if self.task == "feature-extraction":
            return [[0.125, 0.25, 0.5]]
        if self.task == "text-classification":
            return [{"score": 0.875} for _ in value]
        return [{"generated_text": list(value) + [{"role": "assistant", "content": "container-ready"}]}]


def pipeline(task, model, **_kwargs):
    assert model
    return FakePipeline(task)
