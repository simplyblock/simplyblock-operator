FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
ENV LANG=en_US.utf8
ENV LANGUAGE=en_US:en
ENV LC_ALL=en_US.utf8
ENV LC_CTYPE=en_US.UTF-8
RUN apt update && \
  apt -y upgrade && \
  apt install -y postgresql-common && \
  sed -i 's/read enter//g' /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh && \
  /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh && \
  apt update && \
  apt -y install \
  wget \
  file \
  sed \
  build-essential \
  swig \
  bc \
  git \
  locales \
  cpio \
  texinfo \
  rsync \
  vim \
  unzip \
  ncurses-dev \
  lzop \
  bison \
  flex \
  graphviz \
  upx \
  pkg-config \
  nano \
  ccache \
  cmake \
  libtool \
  m4 \
  python3-matplotlib \
  python3-numpy \
  python3-argparse-addons \
  python3 \
  libbz2-dev \
  golang \
  docker.io \
  pandoc \
  upx \
  clang \
  rustfmt \
  postgresql-server-dev-18 \
  libssl-dev && \
  rm -rf /var/lib/apt-lists/* && \
  locale-gen en_US.UTF-8 && \
  echo 'LANG="en_US.UTF-8"' > /etc/default/locale && \
  echo 'export TERM=xterm' >> /root/.bashrc && \
  echo "set -g status off" >> /root/.tmux.conf && \
  echo "export NCURSES_NO_UTF8_ACS=1" >> /root/.bashrc && \
  git config --global user.email "builder@simplyblock.io" && \
  git config --global user.name "vela builder" && \
  mkdir -p /opt/builder && \
  apt-get clean autoclean && \
  apt-get autoremove --yes && \
  rm -rf /var/lib/{apt,dpkg,cache,log}/
