#!/bin/bash
# Builds the KV connector runtime a prefill/decode pair needs, for a vLLM base
# that does not ship it, and leaves its wheel in $1 (empty on every other base).
#
# vLLM's NixlConnector imports `nixl` on CUDA and `rixl` on ROCm (AMD's port of
# NIXL, github.com/ROCm/RIXL). The CUDA bases already carry nixl; the ROCm RDNA
# base carries the connector code with no runtime under it ("NIXL is not
# available"), and the PyPI nixl wheel is CUDA-only. So on a ROCm base this
# compiles UCX with ROCm and RIXL against it, exactly as vLLM's own
# docker/Dockerfile.rocm does at the base's vLLM commit, and bundles both into
# one wheel. Two differences from that recipe, both read from the base:
#   - ROCm is not under /opt/rocm: the rocm/vllm 0.23 bases install it as the
#     rocm-sdk Python wheels, so the root comes from `rocm-sdk path --root`.
#   - the HIP and HSA runtimes are left out of the wheel (auditwheel --exclude):
#     torch has already loaded the base's own copies by the time vLLM imports
#     rixl, and a second HSA runtime in one process is not a thing to ship.
set -euo pipefail

out=${1:?usage: build-kv-connector.sh <wheel-dir>}
mkdir -p "$out"

rocm=""
if command -v rocm-sdk >/dev/null 2>&1; then
  rocm=$(rocm-sdk path --root)
elif [ -d /opt/rocm ]; then
  rocm=/opt/rocm
fi
if [ -z "$rocm" ]; then
  echo "kv connector: not a ROCm base — nothing to build (CUDA bases ship nixl, others have no connector)"
  exit 0
fi
if python3 -c "import rixl" 2>/dev/null; then
  echo "kv connector: the base already carries rixl — nothing to build"
  exit 0
fi

: "${UCX_COMMIT:?}" "${RIXL_COMMIT:?}"
echo "kv connector: ROCm at $rocm — building UCX ${UCX_COMMIT:0:8} and RIXL ${RIXL_COMMIT:0:8}"
export PATH="$rocm/bin:$PATH" ROCM_PATH="$rocm"
py=$(python3 -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')
arch=$(uname -m)

apt-get update
apt-get install -y --no-install-recommends autoconf automake libtool pkg-config git \
  librdmacm-dev libibverbs-dev libaio-dev
python3 -m pip install --no-cache-dir meson auditwheel patchelf tomlkit

src=$(mktemp -d)
git clone https://github.com/openucx/ucx.git "$src/ucx"
git -C "$src/ucx" checkout --quiet "$UCX_COMMIT"
(cd "$src/ucx" && ./autogen.sh && mkdir build && cd build && ../configure \
  --prefix=/usr/local/ucx --enable-shared --disable-static --disable-doxygen-doc \
  --enable-optimizations --enable-devel-headers --with-rocm="$rocm" --with-verbs --enable-mt \
  && make -j"$(nproc)" && make install)
# A UCX built without its ROCm transports would hand every transfer to the
# host and the pair would look like it works; fail the build instead.
ls /usr/local/ucx/lib/ucx/libuct_rocm.so* >/dev/null

git clone https://github.com/ROCm/RIXL.git "$src/rixl"
git -C "$src/rixl" checkout --quiet "$RIXL_COMMIT"
cd "$src/rixl"
meson setup build --prefix=/usr/local/rixl -Ducx_path=/usr/local/ucx -Drocm_path="$rocm" \
  -Dbuild_tests=false -Dbuild_examples=false
meson compile -C build -j "$(nproc)"
meson install -C build
./contrib/tomlutil.py --wheel-name rixl pyproject.toml
python3 -m pip wheel --no-deps --wheel-dir "$src/wheel" \
  -Csetup-args=-Drocm_path="$rocm" -Csetup-args=-Ducx_path=/usr/local/ucx \
  -Csetup-args=-Dbuild_tests=false -Csetup-args=-Dbuild_examples=false .
LD_LIBRARY_PATH="/usr/local/ucx/lib:$rocm/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" \
  auditwheel repair --exclude 'libamdhip64*' --exclude 'libhsa-runtime64*' \
  --exclude 'libcore*' --exclude 'libpull*' \
  --plat "manylinux_2_39_$arch" --wheel-dir "$src/dist" "$src"/wheel/rixl*.whl
./contrib/wheel_add_ucx_plugins.py --ucx-plugins-dir /usr/local/ucx/lib/ucx \
  --nixl-plugins-dir "/usr/local/rixl/lib/$arch-linux-gnu/plugins" "$src"/dist/*.whl
cp "$src"/dist/*.whl "$out"/
echo "kv connector: built $(ls "$out") for python $py"
