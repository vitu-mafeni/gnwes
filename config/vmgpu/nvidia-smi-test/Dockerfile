# A minimal real-GPU-workload smoke test for the Exclusive tier: bundles
# nvidia-smi plus the one userspace library it dlopen()s at runtime
# (libnvidia-ml.so.1 -- NOT a direct ELF dependency, so `ldd nvidia-smi`
# never lists it; find it separately, e.g. `find / -name 'libnvidia-ml.so*'`).
#
# Both files are copied in from the host (see README.md in this
# directory) at the exact driver version this cluster's guest kernel
# module was built against -- NVIDIA requires the kernel module and
# userspace libraries to be the exact same version, a private
# versioned ioctl interface. Copying from the host is a convenience,
# not a requirement: see build-nvidia-kata-image.sh's own note on why
# the guest driver version does not actually depend on the host's.
#
# ubuntu:26.04 to match this cluster's guest rootfs userland
# (config/vmgpu/build-nvidia-kata-image.sh step 5's GUEST_UBUNTU_CODENAME),
# not the host's own distro (confirmed 22.04 on dcn-gpu-0) -- the two
# can differ, and matching the GUEST is what matters for glibc ABI
# compatibility with whatever container actually runs.
FROM ubuntu:26.04

COPY nvidia-smi /usr/bin/nvidia-smi
COPY libnvidia-ml.so.REAL_VERSION /usr/lib/x86_64-linux-gnu/libnvidia-ml.so.REAL_VERSION
RUN chmod 755 /usr/bin/nvidia-smi \
    && ln -s libnvidia-ml.so.REAL_VERSION /usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1 \
    && ln -s libnvidia-ml.so.1 /usr/lib/x86_64-linux-gnu/libnvidia-ml.so \
    && ldconfig

COPY entrypoint.sh /entrypoint.sh
RUN chmod 755 /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
