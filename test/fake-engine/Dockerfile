# The engine is a fully static Go binary, so scratch keeps the fixture
# hermetic and avoids a registry dependency during ordinary L3 runs.
FROM scratch
ADD aliases.tar /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/fake-engine"]
