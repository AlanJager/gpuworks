class AutoModel:
    def __init__(self, model, **_kwargs):
        assert model

    def generate(self, input, **_kwargs):
        with open(input, "rb") as audio:
            prefix = audio.read(4)
        return [{"text": "container-transcript:" + prefix.decode("ascii")}]
