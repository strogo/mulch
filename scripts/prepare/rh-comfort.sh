#!/bin/bash

# -- Run with sudo privileges
# For: CentOS 7

# TODO:
# powerline?

sudo yum -y install mc mlocate man || exit $?

sudo bash -c "cat > /usr/share/mc/mc.ini" <<- EOS
[Midnight-Commander]
use_internal_edit=1
editor_edit_confirm_save=0
confirm_exit=0

[Panels]
navigate_with_arrows=1
EOS
[ $? -eq 0 ] || exit $?


sualias="$_APP_USER"
rh=$(cat /etc/redhat-release)
sudo bash -c "cat > /etc/motd" <<- EOS

$rh: $_VM_NAME
    Generated by Mulch $_MULCH_VERSION on $_VM_INIT_DATE by $_KEY_DESC
    Switch to application user: sudo -iu $_APP_USER (alias: $sualias)

EOS
[ $? -eq 0 ] || exit $?

sudo bash -c "cat > /etc/profile.d/mulch.sh" <<- EOS
if ! shopt -oq posix; then
  alias $sualias="sudo -iu $_APP_USER"
  alias e="mcedit"
fi
EOS
[ $? -eq 0 ] || exit $?

# Powerline
sudo yum -y install epel-release || exit $?
sudo yum -y install python-pip python-pygit2 || exit $?
sudo pip install powerline-status || exit $?

sudo bash -c "cat > /etc/profile.d/powerline.sh" <<- 'EOS'
if ! shopt -oq posix; then
  powerline_sh=$(find /usr/lib -name powerline.sh |grep bash)
  if [ -f "$powerline_sh" ]; then
    . "$powerline_sh"
  fi
fi
EOS
[ $? -eq 0 ] || exit $?

# add a "open" action (see "do" command) if there's any domain defined
if [ -n "$_DOMAIN_FIRST" ]; then
    echo "_MULCH_ACTION_NAME=open"
    echo "_MULCH_ACTION_SCRIPT=https://raw.githubusercontent.com/OnitiFR/mulch/master/scripts/actions/open.sh"
    echo "_MULCH_ACTION_USER=app"
    echo "_MULCH_ACTION_DESCRIPTION=Open VM first domain in the browser"
    echo "_MULCH_ACTION=commit"
fi
