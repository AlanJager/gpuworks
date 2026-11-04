"""Register allocation bases for Mooncake IPC without changing transfer offsets."""
import ctypes
from functools import lru_cache


@lru_cache(maxsize=1)
def _address_query():
    library = ctypes.CDLL("libcuda.so.1")
    query = library.cuMemGetAddressRange_v2
    query.argtypes = [ctypes.POINTER(ctypes.c_uint64), ctypes.POINTER(ctypes.c_size_t), ctypes.c_uint64]
    query.restype = ctypes.c_int
    return library, query


def normalize_registration_regions(regions):
    _, query = _address_query()
    normalized, seen = [], set()
    for pointer, length in regions:
        if length == 0:
            continue
        if pointer <= 0 or length < 0:
            raise ValueError(f"Invalid PPU IPC registration region: {pointer}, {length}")
        base, allocation_size = ctypes.c_uint64(), ctypes.c_size_t()
        result = query(ctypes.byref(base), ctypes.byref(allocation_size), pointer)
        if result:
            raise RuntimeError(f"PPU IPC allocation query failed: pointer={pointer:#x}, code={result}")
        if not (base.value <= pointer and pointer + length <= base.value + allocation_size.value):
            raise ValueError("PPU IPC region extends beyond its CUDA allocation")
        region = (base.value, allocation_size.value)
        if region not in seen:
            seen.add(region)
            normalized.append(region)
    return normalized
