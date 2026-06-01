FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# Added curl to the installation stack
RUN apt-get update && apt-get install -y \
    golang-go \
    ffmpeg \
    curl \
    libgl1-mesa-dri \
    libgl1 \
    libglx-mesa0 \
    libx11-6 \
    libxext6 \
    libxv1 \
    alsa-utils \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

EXPOSE 22050/udp

CMD ["/bin/bash"]
