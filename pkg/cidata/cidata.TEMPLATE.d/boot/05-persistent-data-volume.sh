#!/bin/bash
# bash is used for enabling `set -o pipefail`.
# NOTE: On Alpine, /bin/bash is ash with ASH_BASH_COMPAT, not GNU bash
set -eux -o pipefail

# Restrict the rest of this script to Alpine until it has been tested with other distros
test -f /etc/alpine-release || exit 0

# Data directories that should be persisted across reboots
DATADIRS="/etc /home /root /tmp /usr/local /var/lib"

# When running from RAM try to move persistent data to data-volume
# FIXME: the test for tmpfs mounts is probably Alpine-specific
if [ "$(awk '$2 == "/" {print $3}' /proc/mounts)" == "tmpfs" ]; then
	mkdir -p /mnt/data
	if [ -e /dev/disk/by-label/data-volume ]; then
		mount -t ext4 /dev/disk/by-label/data-volume /mnt/data
	else
		# Find an unpartitioned disk and create data-volume
		DISKS=$(lsblk --list --noheadings --output name,type | awk '$2 == "disk" {print $1}')
		for DISK in ${DISKS}; do
			IN_USE=false
			# Looking for a disk that is not mounted or partitioned
			# shellcheck disable=SC2013
			for PART in $(awk '/^\/dev\// {gsub("/dev/", ""); print $1}' /proc/mounts); do
				if [ "${DISK}" == "${PART}" ] || [ -e /sys/block/"${DISK}"/"${PART}" ]; then
					IN_USE=true
					break
				fi
			done
			if [ "${IN_USE}" == "false" ]; then
				echo 'type=83' | sfdisk --label dos /dev/"${DISK}"
				PART=$(lsblk --list /dev/"${DISK}" --noheadings --output name,type | awk '$2 == "part" {print $1}')
				mkfs.ext4 -L data-volume /dev/"${PART}"
				mount -t ext4 /dev/disk/by-label/data-volume /mnt/data
				# Unmount all mount points under /tmp so we can move it to the data volume:
				# "mount1 on /tmp/lima type 9p (rw,dirsync,relatime,mmap,access=client,trans=virtio)"
				for MP in $(mount | awk '$3 ~ /^\/tmp\// {print $3}'); do
					umount "${MP}"
				done
				for DIR in ${DATADIRS}; do
					DEST="/mnt/data$(dirname "${DIR}")"
					mkdir -p "${DIR}" "${DEST}"
					mv "${DIR}" "${DEST}"
				done
				# Make sure all data moved to the persistent volume has been committed to disk
				sync
				break
			fi
		done
	fi
	for DIR in ${DATADIRS}; do
		if [ -d /mnt/data"${DIR}" ]; then
			mkdir -p "${DIR}"
			mount --bind /mnt/data"${DIR}" "${DIR}"
		fi
	done
	# Make sure to re-mount any mount points under /tmp
	mount -a
fi
