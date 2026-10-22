"""Serve adapters gpuworks ships inside macOS host environment packages.

An inference template starts one model per instance and the gateway rewrites
every request's "model" to the deployment's served name. Servers that pick
the model per request (mlx-audio, mlx-vlm) would try to load that name; the
adapters pin each instance to the model it was started with.
"""
