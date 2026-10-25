"""Backport upstream vLLM's pinned staging of async_copy_to_gpu (build time).

vLLM releases whose Model Runner V2 still has `async_copy_to_gpu` copy step
inputs to the GPU with a non_blocking copy straight from pageable memory. On
ROCm (seen on a Radeon RX 7900 XTX with the vLLM 0.23 RDNA image) that copy
intermittently leaves `query_start_loc` stale and Qwen3 answers garbage after
the first request. Upstream main stages the copy through pinned memory since
vllm-project/vllm#56888; the same change for the RDNA release is
ROCm/vllm#1403. This applies it where the old code is present and does
nothing on any other vLLM.
"""
import importlib.util
import os
import sys

spec = importlib.util.find_spec("vllm")
if spec is None or not spec.submodule_search_locations:
    sys.exit(0)  # no vLLM in this base
path = os.path.join(spec.submodule_search_locations[0], "v1", "worker", "gpu", "buffer_utils.py")
if not os.path.exists(path):
    sys.exit(0)
src = open(path).read()
old = "    return out.copy_(x, non_blocking=True)\n"
if "def async_copy_to_gpu(" not in src or "pin_memory=True).copy_(x)" in src or src.count(old) != 1:
    sys.exit(0)  # a release without the function, already fixed, or not the shape we know
src = src.replace(old, (
    "    if not x.is_pinned() and is_pin_memory_available():\n"
    "        x = torch.empty(x.shape, dtype=x.dtype, device=\"cpu\", pin_memory=True).copy_(x)\n"
    + old))
src = src.replace("from vllm.utils.platform_utils import is_uva_available",
                  "from vllm.utils.platform_utils import is_pin_memory_available, is_uva_available", 1)
open(path, "w").write(src)
print("staged pageable H2D copies in", path)
