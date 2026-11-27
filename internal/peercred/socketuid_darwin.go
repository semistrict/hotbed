//go:build darwin && cgo

package peercred

/*
#include <arpa/inet.h>
#include <libproc.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/proc_info.h>
#include <unistd.h>

static int pid_owns_tcp(int pid, int is_v6, const uint8_t *laddr, uint16_t lport, const uint8_t *faddr, uint16_t fport) {
	int fdbufsize = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, NULL, 0);
	if (fdbufsize <= 0) {
		return 0;
	}
	struct proc_fdinfo *fds = (struct proc_fdinfo *)malloc((size_t)fdbufsize);
	if (fds == NULL) {
		return 0;
	}
	int actual = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, fdbufsize);
	int nfds = actual / (int)sizeof(struct proc_fdinfo);
	int found = 0;
	for (int j = 0; j < nfds; j++) {
		if (fds[j].proc_fdtype != PROX_FDTYPE_SOCKET) {
			continue;
		}
		struct socket_fdinfo si;
		if (proc_pidfdinfo(pid, fds[j].proc_fd, PROC_PIDFDSOCKETINFO, &si, (int)sizeof(si)) != (int)sizeof(si)) {
			continue;
		}
		if (si.psi.soi_kind != SOCKINFO_TCP) {
			continue;
		}
		const struct in_sockinfo *ini = &si.psi.soi_proto.pri_tcp.tcpsi_ini;
		if (ntohs((uint16_t)ini->insi_lport) != lport || ntohs((uint16_t)ini->insi_fport) != fport) {
			continue;
		}
		if (!is_v6) {
			if (ini->insi_vflag & INI_IPV4) {
				if (memcmp(&ini->insi_laddr.ina_46.i46a_addr4, laddr, 4) == 0 &&
				    memcmp(&ini->insi_faddr.ina_46.i46a_addr4, faddr, 4) == 0) {
					found = 1;
					break;
				}
			} else if (ini->insi_vflag & INI_IPV6) {
				static const uint8_t v4mapped[12] = {0,0,0,0, 0,0,0,0, 0,0,0xff,0xff};
				const uint8_t *la = (const uint8_t *)&ini->insi_laddr.ina_6;
				const uint8_t *fa = (const uint8_t *)&ini->insi_faddr.ina_6;
				if (memcmp(la, v4mapped, 12) == 0 && memcmp(la + 12, laddr, 4) == 0 &&
				    memcmp(fa, v4mapped, 12) == 0 && memcmp(fa + 12, faddr, 4) == 0) {
					found = 1;
					break;
				}
			}
		} else if (ini->insi_vflag & INI_IPV6) {
			if (memcmp(&ini->insi_laddr.ina_6, laddr, 16) == 0 &&
			    memcmp(&ini->insi_faddr.ina_6, faddr, 16) == 0) {
				found = 1;
				break;
			}
		}
	}
	free(fds);
	return found;
}

static int darwin_uid_owns_tcp(uint32_t uid, int is_v6, const uint8_t *laddr, uint16_t lport, const uint8_t *faddr, uint16_t fport) {
	int self = getpid();
	if (pid_owns_tcp(self, is_v6, laddr, lport, faddr, fport)) {
		return 1;
	}
	int nbytes = proc_listpids(PROC_UID_ONLY, uid, NULL, 0);
	if (nbytes <= 0) {
		return 0;
	}
	int bufsize = nbytes + (int)(64 * sizeof(int));
	int *pids = (int *)malloc((size_t)bufsize);
	if (pids == NULL) {
		return 0;
	}
	int actual = proc_listpids(PROC_UID_ONLY, uid, pids, bufsize);
	int npids = actual / (int)sizeof(int);
	int found = 0;
	for (int i = 0; i < npids; i++) {
		int pid = pids[i];
		if (pid <= 0 || pid == self) {
			continue;
		}
		if (pid_owns_tcp(pid, is_v6, laddr, lport, faddr, fport)) {
			found = 1;
			break;
		}
	}
	free(pids);
	return found;
}
*/
import "C"

import (
	"fmt"
	"net/netip"
	"os"
	"unsafe"
)

// SocketUID is the unix user that opened the connection from client, on
// this machine, to server. On macOS, an unprivileged daemon can inspect
// sockets of processes owned by its own UID, which is all the caller check
// needs (same user as the daemon).
func SocketUID(client, server netip.AddrPort) (uint32, error) {
	uid := uint32(os.Getuid())
	ca := client.Addr().Unmap()
	sa := server.Addr().Unmap()
	if ca.Is4() != sa.Is4() {
		return 0, fmt.Errorf("address family mismatch between %s and %s", client, server)
	}
	var isV6 C.int
	var lbytes, fbytes []byte
	if ca.Is4() {
		l4 := ca.As4()
		f4 := sa.As4()
		lbytes = l4[:]
		fbytes = f4[:]
	} else {
		isV6 = 1
		l16 := ca.As16()
		f16 := sa.As16()
		lbytes = l16[:]
		fbytes = f16[:]
	}
	if C.darwin_uid_owns_tcp(
		C.uint32_t(uid),
		isV6,
		(*C.uint8_t)(unsafe.Pointer(&lbytes[0])),
		C.uint16_t(client.Port()),
		(*C.uint8_t)(unsafe.Pointer(&fbytes[0])),
		C.uint16_t(server.Port()),
	) != 0 {
		return uid, nil
	}
	return 0, fmt.Errorf("no TCP socket %s -> %s owned by uid %d", client, server, uid)
}
