class FakeImage:
    def save(self, target, format):
        assert format == "PNG"
        target.write(b"PNG-container")


class FakeOutput:
    images = [FakeImage()]
    frames = [["frame"]]


class FakePipeline:
    def to(self, _device):
        return self

    def __call__(self, prompt, **_kwargs):
        assert prompt
        return FakeOutput()


class DiffusionPipeline:
    @staticmethod
    def from_pretrained(model, **_kwargs):
        assert model
        return FakePipeline()
