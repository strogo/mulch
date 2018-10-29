#!/bin/bash

# -- Run with sudo privileges
# For: Debian 9 / Ubuntu 18.10

# Unlike RedHat/CentOS, Debian does not source profile for non-login shells:
. /etc/mulch.env

export DEBIAN_FRONTEND="noninteractive"
sudo -E apt-get -y -qq install progress mc powerline locate man || exit $?

sualias="su-$_APP_USER"
sudo bash -c "cat > /etc/motd" <<- EOS

Debian GNU/Linux: $_VM_NAME
    Generated by Mulch $_MULCH_VERSION on $_VM_INIT_DATE by $_KEY_DESC
    Switch to application user: sudo -iu $_APP_USER (alias: $sualias)

EOS
[ $? -eq 0 ] || exit $?

sudo bash -c "cat > /etc/profile.d/mulcj.sh" <<- EOS
if ! shopt -oq posix; then
  alias $sualias="sudo -iu $_APP_USER"
  alias ll="ls -la --color"
  alias e="mcedit"
fi
EOS
[ $? -eq 0 ] || exit $?

sudo bash -c "cat > /etc/profile.d/powerline.sh" <<- EOS
if ! shopt -oq posix; then
  if [ -f /usr/share/powerline/bindings/bash/powerline.sh ]; then
    . /usr/share/powerline/bindings/bash/powerline.sh
  fi
fi
EOS
[ $? -eq 0 ] || exit $?

# remove public access for home directories
sudo chmod o= /home/*/ /etc/skel || exit $?
