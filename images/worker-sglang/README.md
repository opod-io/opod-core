# opod-worker-sglang

SGLang, wrapped the same way as every other worker: the official image brings the
engine, we add `opod` and the shared entrypoint. The worker runs `opod join`, and
the agent starts `python3 -m sglang.launch_server` when the leader asks it to
hold a model (`internal/agent/server.go`, `launchSGLang`).

Why an operator would choose it: RadixAttention, a prefix cache that survives
across requests. That is also why the driver reads `sglang:cache_hit_rate` —
a fleet where the hit rate collapses has lost the reason it is running SGLang.

```bash
# NVIDIA
images/build.sh --push --latest sglang-nvidia
# AMD (ROCm base; the image is not published yet — build it where the cards are)
BASE=lmsysorg/sglang:v0.5.2-rocm630 images/build.sh --push sglang-amd
```

Not supported: the sleep tier. SGLang has no sleep mode, so an endpoint that
asks for one is refused at plan time rather than parked and never woken.
