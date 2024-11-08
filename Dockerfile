# Use the latest stable Debian image
FROM debian:bookworm-slim

# Environment variable for AMC version, replace with your required version
ENV AMC_VERSION=5.0.0

# Update package list, install necessary packages, download AMC, and perform cleanup
RUN apt-get update && \
    apt-get install -y --no-install-recommends wget procps && \
    wget https://github.com/aerospike-community/amc/releases/download/${AMC_VERSION}/aerospike-amc-enterprise-${AMC_VERSION}_amd64.deb --no-check-certificate && \
    dpkg -i aerospike-amc-enterprise-${AMC_VERSION}_amd64.deb && \
    rm aerospike-amc-enterprise-${AMC_VERSION}_amd64.deb && \
    apt-get purge -y wget && \
    apt-get autoremove -y && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Copy necessary scripts or files
COPY ./deployment/common/amc.docker.sh /opt/amc/amc.docker.sh

EXPOSE 8081

ENTRYPOINT [ "/opt/amc/amc.docker.sh", "amc" ]
