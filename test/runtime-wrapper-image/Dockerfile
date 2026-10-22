ARG GPUWORKS_L3_RUNTIME_BASE_IMAGE=python:3.12-slim
FROM ${GPUWORKS_L3_RUNTIME_BASE_IMAGE}

COPY fake_deps/ /opt/fake-deps/
ENV PYTHONPATH=/opt/fake-deps

# The image deliberately contains framework-like dependencies only. The
# GPUWorks HTTP wrapper is injected later from the catalog asset mount.
ENTRYPOINT ["python3"]
