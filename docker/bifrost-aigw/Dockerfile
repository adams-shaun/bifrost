FROM volterra.azurecr.io/ves.io/go-builder:v2.6.0 AS ubi-micro-build

RUN mkdir -p /mnt/rootfs

# Install minimal runtime dependencies on /mnt/rootfs
RUN yum install --installroot /mnt/rootfs \
    coreutils-single \
    glibc-minimal-langpack \
    curl \
    wget \
    --releasever 9 --setopt install_weak_deps=false --nodocs -y

RUN yum --installroot /mnt/rootfs clean all && \
    rm -rf /mnt/rootfs/var/cache/* /mnt/rootfs/var/log/dnf* /mnt/rootfs/var/log/yum.*

RUN wget https://github.com/krallin/tini/releases/download/v0.19.0/tini && \
    chmod +x tini

FROM volterra.azurecr.io/ves.io/f5-fips-ubi-micro:v3.3.7
USER root

COPY --from=ubi-micro-build /mnt/rootfs/ /
COPY --from=ubi-micro-build /opt/app-root/src/tini /sbin/tini
COPY --from=ubi-micro-build /usr/lib64/libssl.so.3.5.1 /usr/lib64/libcrypto.so.3.5.1 /usr/lib64/libz.so.1.2.11 /usr/lib64/

# RUN ln -f -s /usr/lib64/libcrypto.so.3.5.1  /usr/lib64/libcrypto.so && \
#     ln -f -s /usr/lib64/libcrypto.so.3.5.1  /usr/lib64/libcrypto.so.3.0 && \
#     ln -f -s /usr/lib64/libz.so.1.2.11  /usr/lib64/libz.so && \
#     ln -f -s /usr/lib64/libz.so.1.2.11  /usr/lib64/libz.so.1 && \
#     ln -f -s /usr/lib64/libssl.so.3.5.1  /usr/lib64/libssl.so.3.0 && \
#     ln -f -s /usr/lib64/libssl.so.3.5.1  /usr/lib64/libssl.so

# Copy pre-built binary (built by CI build step outside Docker)
COPY build_out/bifrost-aigw /bin/bifrost-aigw
COPY VERSION /

ADD deploy/bifrost-aigw/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Create app data directory
RUN mkdir -p /app/data/logs

# Remove setuid/setgid bits for security
RUN find / -perm /6000 -type f -exec chmod a-s {} \; || true

EXPOSE 8080

ENTRYPOINT ["/sbin/tini", "--", "/entrypoint.sh"]
