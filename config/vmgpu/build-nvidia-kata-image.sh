# Builds a custom, driver-included Kata guest kernel + rootfs for the
# Exclusive tier, so a hot-attached GPU can actually be used (nvidia.ko
# loaded and bound), not just enumerated at the PCI level.
#
# Confirmed live (2026-11-15) on dcn-gpu-0. This captures the exact
# process worked out interactively on that machine -- see
# config/vmgpu/README.md's 2026-11-15 "custom Kata GPU image" entry for
# the full narrative (why NVRC/kata-deploy's official NVIDIA shims don't
# apply here: they're QEMU-only, static-assignment-at-creation, and this
# project specifically needs cloud-hypervisor + live hot-attach).
#
# Run on the GPU node itself (needs its exact installed NVIDIA driver
# version to match the kernel module we build -- NVIDIA kernel modules
# and userspace libraries must be the EXACT same version, they share a
# private versioned ioctl interface). Needs: git, docker (with sudo
# access), gcc/make/bc/bison/flex/libelf-dev/libssl-dev, ~5GB free disk,
# outbound internet (kernel.org, github.com, archive.ubuntu.com, and
# this cluster's rustfs endpoint).
#
# Run as: bash build-nvidia-kata-image.sh
set -euo pipefail

WORKDIR="${WORKDIR:-$HOME/kata-gpu-build}"
KATA_TAG="${KATA_TAG:-4.3.1}"                    # must match defaultKataVersion in
                                                   # agent-substrate's cmd/ate-setup/internal/steps/microvm.go
ARCH="${ARCH:-x86_64}"
HYPERVISOR="${HYPERVISOR:-cloud-hypervisor}"      # NOT qemu -- this is the whole point: keeping
                                                   # live hot-attach, which kata-deploy's official
                                                   # NVIDIA shims (qemu-nvidia-gpu, QEMU-only,
                                                   # static GPU-at-creation) do not provide.
RUSTFS_ENDPOINT="${RUSTFS_ENDPOINT:-http://10.9.0.13:31429}"
RUSTFS_BUCKET="${RUSTFS_BUCKET:-ate-snapshots}"
AWS_CLI_IMAGE="amazon/aws-cli:2.38.5@sha256:c8156a0e74e13929a42d7e60f0d4e14e7d28d980cd21822505d4bff80e5460c1"

echo "== 1. clone kata-containers at the version this cluster already uses =="
mkdir -p "$WORKDIR" && cd "$WORKDIR"
if [ ! -d kata-containers ]; then
  git clone --depth 1 --branch "$KATA_TAG" https://github.com/kata-containers/kata-containers.git
fi
cd kata-containers

echo "== 2. pin the NVIDIA kernel-module source to THIS HOST's exact installed driver version =="
# versions.yaml ships pinned to whatever upstream last set (confirmed
# 2026-11-15: that was 615.71.09, newer than any driver this host has).
# NVIDIA kernel modules and userspace libraries MUST be the exact same
# version -- building against the wrong one fails at module init, not
# at compile time, so get this right before building anything.
HOST_DRIVER_VERSION=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -1)
echo "host driver version: $HOST_DRIVER_VERSION"
if ! curl -fs "https://api.github.com/repos/NVIDIA/open-gpu-kernel-modules/tags?per_page=100" \
     | python3 -c "import json,sys; print('\n'.join(t['name'] for t in json.load(sys.stdin)))" \
     | grep -qx "$HOST_DRIVER_VERSION"; then
  echo "ERROR: open-gpu-kernel-modules has no tag matching $HOST_DRIVER_VERSION -- it must be driver source NVIDIA hasn't published as open-source yet. Check manually before proceeding." >&2
  exit 1
fi
sed -i "s/version: \"[0-9.]*\"/version: \"${HOST_DRIVER_VERSION}\"/" versions.yaml
grep -A4 "^  nvidia:" versions.yaml | grep version

echo "== 3. build the kernel (Kata's own -g nvidia flag merges the right config fragments and fetches the matching driver source automatically) =="
cd tools/packaging/kernel
bash build-kernel.sh -a "$ARCH" -t "$HYPERVISOR" -g nvidia -f setup
bash build-kernel.sh -a "$ARCH" -t "$HYPERVISOR" -g nvidia build
KERNEL_DIR=$(find . -maxdepth 1 -iname "kata-linux-*" -type d | head -1)
KERNEL_RELEASE=$(basename "$(find "$KERNEL_DIR/lib/modules" -maxdepth 1 -mindepth 1 -type d | head -1)")
echo "kernel release: $KERNEL_RELEASE (vmlinux: $KERNEL_DIR/vmlinux)"

echo "== 4. fetch the exact stock rootfs this cluster already deploys, as the base to customize =="
STOCK_KERNEL_URL=$(kubectl get sandboxconfig microvm -o jsonpath='{.spec.versions[0].assets.'"$([ "$ARCH" = x86_64 ] && echo amd64 || echo "$ARCH")"'.kata-image.url}')
STOCK_SHA=$(basename "$STOCK_KERNEL_URL")
mkdir -p "$WORKDIR/stock-rootfs" && cd "$WORKDIR/stock-rootfs"
if [ ! -f rootfs.img ]; then
  sudo docker run --rm \
    -e AWS_ACCESS_KEY_ID=rustfsadmin -e AWS_SECRET_ACCESS_KEY=rustfsadmin -e AWS_REGION=us-east-1 \
    -e AWS_ENDPOINT_URL="$RUSTFS_ENDPOINT" --network host \
    -v "$WORKDIR/stock-rootfs:/out" "$AWS_CLI_IMAGE" \
    s3 cp "s3://${RUSTFS_BUCKET}/microvm-assets/amd64/${STOCK_SHA}" /out/rootfs.img
fi
[ "$(sha256sum rootfs.img | cut -d' ' -f1)" = "$STOCK_SHA" ] || { echo "ERROR: stock rootfs sha256 mismatch" >&2; exit 1; }

echo "== 5. get kmod + libkmod matching the GUEST's distro (NOT the host's -- ABI must match the guest's libc) =="
GUEST_UBUNTU_CODENAME=$(mkdir -p /tmp/.rootfs-peek && sudo mount -o loop,ro "$(losetup -fP --show rootfs.img)" /tmp/.rootfs-peek 2>/dev/null; . /tmp/.rootfs-peek/etc/os-release 2>/dev/null; echo "$VERSION_ID"; sudo umount /tmp/.rootfs-peek 2>/dev/null; sudo losetup -D 2>/dev/null)
mkdir -p "$WORKDIR/kmod-pkg" && cd "$WORKDIR/kmod-pkg"
if [ ! -f kmod-extracted/usr/bin/kmod ]; then
  sudo docker run --rm -v "$PWD:/out" "ubuntu:${GUEST_UBUNTU_CODENAME:-26.04}" bash -c \
    "apt-get update -qq && apt-get install -y --no-install-recommends --download-only -o Dir::Cache::archives=/out kmod"
  for deb in *.deb; do dpkg-deb -x "$deb" kmod-extracted; done
  sudo chown -R root:root kmod-extracted
fi

echo "== 6. assemble the custom rootfs: stock image + kmod/libkmod + our modules + autoload config =="
cp "$WORKDIR/stock-rootfs/rootfs.img" "$WORKDIR/stock-rootfs/rootfs-nvidia.img"
LOOPDEV=$(sudo losetup -fP --show "$WORKDIR/stock-rootfs/rootfs-nvidia.img")
sudo mkdir -p /mnt/nvidia-rootfs
sudo mount -o rw "${LOOPDEV}p1" /mnt/nvidia-rootfs

sudo cp -a "$WORKDIR/kmod-pkg/kmod-extracted/usr/bin/kmod" /mnt/nvidia-rootfs/usr/bin/kmod
sudo cp -a "$WORKDIR/kmod-pkg/kmod-extracted/usr/lib/x86_64-linux-gnu/libkmod.so.2."* /mnt/nvidia-rootfs/usr/lib/x86_64-linux-gnu/
LIBKMOD_REAL=$(basename "$(find "$WORKDIR/kmod-pkg/kmod-extracted/usr/lib/x86_64-linux-gnu" -name 'libkmod.so.2.*')")
sudo ln -sf "$LIBKMOD_REAL" /mnt/nvidia-rootfs/usr/lib/x86_64-linux-gnu/libkmod.so.2
for tool in modprobe modinfo insmod lsmod depmod rmmod; do
  sudo ln -sf /usr/bin/kmod "/mnt/nvidia-rootfs/usr/sbin/${tool}"
done
sudo ln -sf /usr/bin/kmod /mnt/nvidia-rootfs/usr/bin/lsmod
sudo mkdir -p /mnt/nvidia-rootfs/etc/depmod.d
sudo cp -a "$WORKDIR/kmod-pkg/kmod-extracted/etc/depmod.d/ubuntu.conf" /mnt/nvidia-rootfs/etc/depmod.d/

# Exact, explicit file copies -- NOT a whole-directory `cp -a`: the
# build tree's modules dir contains a `build` symlink back to the
# kernel source tree, and a recursive copy (or any later directory
# merge) follows it and pulls in the ENTIRE kernel source (100+MB) --
# confirmed the hard way (2026-11-15). Only kernel/ + modules.* belong
# in the guest.
SRC="$WORKDIR/kata-containers/tools/packaging/kernel/${KERNEL_DIR#./}/lib/modules/${KERNEL_RELEASE}"
DST="/mnt/nvidia-rootfs/usr/lib/modules/${KERNEL_RELEASE}"
sudo rm -rf "$DST"
sudo mkdir -p "$DST/kernel/drivers/video"
sudo cp "$SRC/kernel/drivers/video/"*.ko "$DST/kernel/drivers/video/"
sudo cp "$SRC"/modules.* "$DST/"

sudo mkdir -p /mnt/nvidia-rootfs/etc/modules-load.d
printf 'nvidia\nnvidia-uvm\nnvidia-modeset\nnvidia-drm\n' | sudo tee /mnt/nvidia-rootfs/etc/modules-load.d/nvidia.conf >/dev/null
# nvidia-peermem deliberately omitted: GPUDirect RDMA only, not needed
# for a single-GPU actor and not auto-loaded by default upstream either.
# /etc/modules-load.d/nvidia.conf above is only honored by
# systemd-modules-load.service -- harmless to leave in place, but
# INERT on this cluster (see install_init_wrapper below), so don't
# rely on it alone.

echo "== 6b. install_init_wrapper: this rootfs's /usr/sbin/init -> systemd is never actually run here =="
# Confirmed via a genuine cold-boot console log (2026-11-15): this
# cluster's kernel is booted with init=/usr/bin/kata-agent directly
# ("[ 0.205443] check access for rdinit=/init failed: -2, ignoring"
# immediately followed by "[ 0.208384] Run /usr/bin/kata-agent as init
# process"), bypassing systemd -- and therefore
# systemd-modules-load.service -- entirely. Without this step the
# modules above are present in the image but never loaded, and
# nvidia.ko never binds despite the hot-attach itself succeeding.
# Same technique NVIDIA's own NVRC init uses: rename the real init
# aside, install a tiny wrapper at the exact path the kernel execs as
# init, modprobe the modules, then exec the real binary -- exec
# replaces the process image in place, so PID 1 is preserved.
if [ ! -f /mnt/nvidia-rootfs/usr/bin/kata-agent.real ]; then
  sudo mv /mnt/nvidia-rootfs/usr/bin/kata-agent /mnt/nvidia-rootfs/usr/bin/kata-agent.real
  cat <<'WRAPPER' | sudo tee /mnt/nvidia-rootfs/usr/bin/kata-agent >/dev/null
#!/bin/sh
for m in nvidia nvidia-uvm nvidia-modeset nvidia-drm; do
  /usr/sbin/modprobe "$m" 2>/dev/null
done
exec /usr/bin/kata-agent.real "$@"
WRAPPER
  sudo chmod 755 /mnt/nvidia-rootfs/usr/bin/kata-agent
fi

df -h /mnt/nvidia-rootfs   # confirm there's still free space -- this project's
                            # stock image ships ~50MB of headroom; our additions
                            # are ~25MB. A future rootfs with less headroom, or a
                            # bigger driver, needs IMAGE_SIZE_ALIGNMENT_MB-aware
                            # resizing (see tools/osbuilder/image-builder), not
                            # attempted here.
sudo umount /mnt/nvidia-rootfs
sudo losetup -d "$LOOPDEV"

echo "== 7. upload both custom assets and create a new SandboxConfig (the stock 'microvm' config is untouched) =="
ROOTFS_SHA=$(sha256sum "$WORKDIR/stock-rootfs/rootfs-nvidia.img" | cut -d' ' -f1)
KERNEL_SHA=$(sha256sum "$WORKDIR/kata-containers/tools/packaging/kernel/${KERNEL_DIR#./}/vmlinux" | cut -d' ' -f1)
echo "rootfs sha256: $ROOTFS_SHA"
echo "kernel sha256: $KERNEL_SHA"

for pair in "rootfs-nvidia.img:$ROOTFS_SHA:$WORKDIR/stock-rootfs/rootfs-nvidia.img" \
            "vmlinux:$KERNEL_SHA:$WORKDIR/kata-containers/tools/packaging/kernel/${KERNEL_DIR#./}/vmlinux"; do
  sha="${pair#*:}"; sha="${sha%%:*}"
  file="${pair##*:}"
  sudo docker run --rm \
    -e AWS_ACCESS_KEY_ID=rustfsadmin -e AWS_SECRET_ACCESS_KEY=rustfsadmin -e AWS_REGION=us-east-1 \
    -e AWS_ENDPOINT_URL="$RUSTFS_ENDPOINT" -e AWS_RETRY_MODE=adaptive -e AWS_MAX_ATTEMPTS=15 \
    --network host -v "$(dirname "$file"):/data" "$AWS_CLI_IMAGE" \
    s3 cp "/data/$(basename "$file")" "s3://${RUSTFS_BUCKET}/microvm-assets/amd64/${sha}" \
    --cli-connect-timeout 60
    # A SlowDown/throttle error from rustfs on a large file is not a
    # real failure -- confirmed 2026-11-15, AWS_RETRY_MODE=adaptive
    # clears it (do NOT reach for skopeo or another tool; it's an S3
    # client/server interaction, nothing to do with container images).
done

cat <<EOF

Now apply this SandboxConfig (reuses this cluster's existing stock
cloud-hypervisor/virtiofsd asset entries unchanged -- fetch them with
\`kubectl get sandboxconfig microvm -o yaml\` if they've since changed):

apiVersion: ate.dev/v1
kind: SandboxConfig
metadata:
  name: microvm-nvidia-gpu
spec:
  defaultVersion: v1
  sandboxClass: microvm
  versions:
  - name: v1
    state: Enabled
    assets:
      amd64:
        cloud-hypervisor: {sha256: <copy from the existing 'microvm' SandboxConfig>, url: <same>}
        virtiofsd: {sha256: <copy from the existing 'microvm' SandboxConfig>, url: <same>}
        kata-kernel:
          sha256: ${KERNEL_SHA}
          url: s3://${RUSTFS_BUCKET}/microvm-assets/amd64/${KERNEL_SHA}
        kata-image:
          sha256: ${ROOTFS_SHA}
          url: s3://${RUSTFS_BUCKET}/microvm-assets/amd64/${ROOTFS_SHA}
EOF
