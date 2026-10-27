"""Explicit 100-repository metadata corpus; never a management API stub."""
from dataclasses import dataclass
import re
from types import MappingProxyType
from urllib.parse import unquote, urlsplit

from model_hub_fixture import CONFIG, INDEX, README, SHA, WEIGHT_BYTES, ModelHub, encoded

# Independent fixed inputs. Stages 10/30/100 contain 5/15/50 of each source.
CORPUS = tuple((i, "huggingface" if i % 2 else "modelscope",
                f"metadata-{i:03d}", f"model-{i:03d}") for i in range(1, 101))


@dataclass(frozen=True)
class Repository:
    number: int
    source: str
    repo: str
    name: str
    revision: str

    def registration_body(self):
        """Real POST /api/v1/models input; server fetches actual HTTP metadata."""
        return {"source": self.source, "source_id": self.repo,
                "revision": self.revision, "name": self.name}

    def expected_public_facts(self):
        return {**self.registration_body(), "arch": "Qwen2ForCausalLM",
                "weight_gib": WEIGHT_BYTES / 1024 ** 3,
                "context_length": 4096, "precision": "float16", "readme": README}


def declared_rows(namespace):
    if not isinstance(namespace, str) or not re.fullmatch(r"e2e-[a-z0-9-]+", namespace):
        raise ValueError("namespace must be an explicit run-owned e2e identity")
    return tuple(Repository(i, source, namespace + "/" + slug, namespace + "-" + name,
                            "main" if source == "huggingface" else "master")
                 for i, source, slug, name in CORPUS)


class MetadataTableHub(ModelHub):
    """One real listener; only the explicitly declared provider/repo is served.

    Reuses the shared repository audited protocol rather than introducing permissive new
    query handling. Unknown providers/repos/refs/files/queries cannot succeed.
    """
    def __init__(self, namespace):
        self.rows = declared_rows(namespace)
        super().__init__(self.rows[0].repo)
        self.repositories = MappingProxyType({row.repo: row for row in self.rows})
        self._protocols = {row.repo: ModelHub(row.repo) for row in self.rows}
        routes = {}
        for row in self.rows:
            prefixes = (("/hf/api/models/", "/hf/") if row.source == "huggingface"
                        else ("/ms/api/v1/models/", "/ms/models/"))
            for prefix in prefixes:
                routes[prefix + row.repo] = row.repo
        self._routes = MappingProxyType(routes)

    def answer(self, method, target, headers=None, body=b""):
        normalized = {k.lower(): v for k, v in (headers or {}).items()}
        if method != "GET" or body or normalized.get("authorization") or normalized.get("cookie"):
            return super().answer(method, target, headers, body)
        path = unquote(urlsplit(target).path)
        for prefix, repo in self._routes.items():
            if path == prefix or path.startswith(prefix + "/"):
                return self._protocols[repo].answer(method, target, headers, body)
        return 404, encoded({"error": "Repository or file not found"})

    def available(self, value):
        super().available(value)
        for protocol in self._protocols.values():
            protocol.available(value)

    def repo_available(self, repo, value):
        if repo not in self.repositories:
            raise ValueError("undeclared repository")
        self._protocols[repo].available(value)
