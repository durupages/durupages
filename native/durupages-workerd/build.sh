#!/usr/bin/env bash
# Copyright 2026 JC-Lab
# SPDX-License-Identifier: EPL-2.0

# Build durupages-workerd: clone the pinned workerd, apply the DuruPages patch
# (which injects the real per-isolate heap-limit enforcer — see 6.6 of the
# architecture doc), and build the server binary with Bazel.
#
# Output: ./bin/durupages-workerd
#
# Requirements: git, a C++20 clang toolchain, an unversioned `tclsh` (sqlite's
# build needs it), and bazelisk/bazel. The script auto-links common versioned
# names (clang-20 / clang++ / lld-20 / tclsh8.6) into a private toolchain dir
# so the build works on stock Ubuntu without those unversioned aliases.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${HERE}"

# shellcheck disable=SC1091
source ./WORKERD_VERSION

WORK="${DURUPAGES_WORKERD_BUILD_DIR:-${HERE}/.build}"
SRC="${WORK}/workerd"
OUT="${HERE}/bin"
# A stable disk cache so re-clones and other checkouts reuse the (very large)
# V8 compilation instead of rebuilding it.
DISK_CACHE="${DURUPAGES_WORKERD_DISK_CACHE:-${HOME}/.cache/durupages-workerd}"

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }

# ---------------------------------------------------------------------------
# Toolchain: provide unversioned clang/clang++/ld.lld/tclsh on PATH.
#
# DURUPAGES_CC / DURUPAGES_CXX pin an exact compiler. Use them when a machine
# already has an unversioned `clang` you do not want (CI runners ship one), or
# when the C++ compiler must match the installed libc++: workerd builds with
# -stdlib=libc++, so a clang whose libc++ dev headers are absent fails every
# translation unit with "'cstddef' file not found".
# ---------------------------------------------------------------------------
TOOLBIN="${WORK}/toolbin"
mkdir -p "${TOOLBIN}"
link_tool() { # link_tool <unversioned> <candidate...>
  local want="$1"; shift
  if command -v "${want}" >/dev/null 2>&1; then return; fi
  local c
  for c in "$@"; do
    if command -v "${c}" >/dev/null 2>&1; then
      ln -sf "$(command -v "${c}")" "${TOOLBIN}/${want}"; return
    fi
  done
  echo "ERROR: none of [${*}] found for required tool '${want}'" >&2; exit 1
}
pin_tool() { # pin_tool <unversioned> <requested>
  local want="$1" req="$2" path
  path="$(command -v "${req}" 2>/dev/null || true)"
  if [[ -z "${path}" ]]; then
    echo "ERROR: requested tool '${req}' not found" >&2; exit 1
  fi
  ln -sf "${path}" "${TOOLBIN}/${want}"
}

if [[ -n "${DURUPAGES_CC:-}" ]]; then
  pin_tool clang "${DURUPAGES_CC}"
else
  link_tool clang clang-20 clang-19 clang-18 clang
fi
if [[ -n "${DURUPAGES_CXX:-}" ]]; then
  pin_tool clang++ "${DURUPAGES_CXX}"
else
  link_tool clang++ clang++-20 clang++-19 clang++-18 clang++
fi
link_tool ld.lld   lld-20 lld-19 lld-18 ld.lld
link_tool tclsh    tclsh8.6 tclsh8.7 tclsh
export PATH="${TOOLBIN}:${PATH}"
CC_BIN="$(command -v clang)"
CXX_BIN="$(command -v clang++)"
log "Using CC=${CC_BIN} CXX=${CXX_BIN}"
"${CC_BIN}" --version | head -1

BAZEL="${DURUPAGES_BAZEL:-bazelisk}"
command -v "${BAZEL}" >/dev/null 2>&1 || BAZEL=bazel

# ---------------------------------------------------------------------------
# Clone + patch workerd at the pinned ref.
# ---------------------------------------------------------------------------
if [[ ! -d "${SRC}/.git" ]]; then
  log "Cloning workerd @ ${WORKERD_REF} (release ${WORKERD_RELEASE})"
  mkdir -p "${WORK}"
  git clone --filter=blob:none https://github.com/cloudflare/workerd.git "${SRC}"
fi
log "Checking out ${WORKERD_REF} and applying DuruPages patch"
git -C "${SRC}" fetch --depth 1 origin "${WORKERD_REF}" 2>/dev/null || true
git -C "${SRC}" checkout -q "${WORKERD_REF}"
git -C "${SRC}" reset -q --hard "${WORKERD_REF}"
git -C "${SRC}" clean -qfd
for p in "${HERE}"/patches/*.patch; do
  log "Applying $(basename "${p}")"
  git -C "${SRC}" apply --whitespace=nowarn "${p}"
done

# ---------------------------------------------------------------------------
# Build.
# ---------------------------------------------------------------------------
log "Building //src/workerd/server:workerd"
JOBS="${DURUPAGES_BUILD_JOBS:-$(( $(nproc) > 8 ? 6 : $(nproc) ))}"
MEM="${DURUPAGES_BUILD_MEM_MB:-7000}"

# workerd pulls in rules_android transitively. Its repository rule reads
# ANDROID_HOME and, when that points at an SDK with no installed API levels,
# fails analysis outright with "No Android SDK apis found". We build no Android
# targets, so blank the variables out: an unset value makes the rule emit an
# empty stub repo instead. GitHub runners set ANDROID_HOME, so without this the
# build breaks there even though it succeeds on a machine that has none.
( cd "${SRC}" && CC="${CC_BIN}" CXX="${CXX_BIN}" "${BAZEL}" build //src/workerd/server:workerd \
    --jobs="${JOBS}" \
    --local_resources=memory="${MEM}" \
    --disk_cache="${DISK_CACHE}" \
    --repo_env=CC="${CC_BIN}" \
    --repo_env=CXX="${CXX_BIN}" \
    --repo_env=BAZEL_COMPILER=clang \
    --repo_env=ANDROID_HOME= \
    --repo_env=ANDROID_SDK_ROOT= \
    --action_env=PATH="${TOOLBIN}:/bin:/usr/bin:/usr/local/bin" \
    --host_action_env=PATH="${TOOLBIN}:/bin:/usr/bin:/usr/local/bin" \
    --linkopt=-fuse-ld=lld )

mkdir -p "${OUT}"
install -m 0755 "${SRC}/bazel-bin/src/workerd/server/workerd" "${OUT}/durupages-workerd"
log "Built ${OUT}/durupages-workerd"
"${OUT}/durupages-workerd" --version || true
