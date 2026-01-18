#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <netdb.h>
#include <unistd.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>

// Function pointers for original syscalls
static int (*real_connect)(int, const struct sockaddr *, socklen_t) = NULL;
static int (*real_getaddrinfo)(const char *, const char *, const struct addrinfo *, struct addrinfo **) = NULL;
static struct hostent *(*real_gethostbyname)(const char *) = NULL;

// Configuration
static char *proxy_host = "127.0.0.1";
static int proxy_port = 1080;
static int initialized = 0;

// Initialize the library
static void init_preload(void) {
    if (initialized) return;

    // Load original function pointers
    real_connect = dlsym(RTLD_NEXT, "connect");
    real_getaddrinfo = dlsym(RTLD_NEXT, "getaddrinfo");
    real_gethostbyname = dlsym(RTLD_NEXT, "gethostbyname");

    // Read proxy configuration from environment
    char *env_port = getenv("TAILPROXY_PORT");
    if (env_port) {
        proxy_port = atoi(env_port);
    }

    char *env_host = getenv("TAILPROXY_HOST");
    if (env_host) {
        proxy_host = env_host;
    }

    initialized = 1;

    if (getenv("TAILPROXY_VERBOSE")) {
        fprintf(stderr, "[tailproxy] Initialized: proxy=%s:%d\n", proxy_host, proxy_port);
    }
}

// SOCKS5 handshake and connect
static int socks5_connect(int sockfd, const struct sockaddr *addr, socklen_t addrlen) {
    unsigned char buf[512];
    int ret;

    // SOCKS5 greeting
    buf[0] = 0x05; // SOCKS version 5
    buf[1] = 0x01; // 1 auth method
    buf[2] = 0x00; // No authentication

    if (send(sockfd, buf, 3, 0) != 3) {
        return -1;
    }

    // Read greeting response
    if (recv(sockfd, buf, 2, 0) != 2) {
        return -1;
    }

    if (buf[0] != 0x05 || buf[1] != 0x00) {
        errno = ECONNREFUSED;
        return -1;
    }

    // Build SOCKS5 connect request
    buf[0] = 0x05; // SOCKS version
    buf[1] = 0x01; // CONNECT command
    buf[2] = 0x00; // Reserved

    int pos = 3;

    if (addr->sa_family == AF_INET) {
        struct sockaddr_in *addr_in = (struct sockaddr_in *)addr;
        buf[pos++] = 0x01; // IPv4
        memcpy(&buf[pos], &addr_in->sin_addr.s_addr, 4);
        pos += 4;
        memcpy(&buf[pos], &addr_in->sin_port, 2);
        pos += 2;
    } else if (addr->sa_family == AF_INET6) {
        struct sockaddr_in6 *addr_in6 = (struct sockaddr_in6 *)addr;
        buf[pos++] = 0x04; // IPv6
        memcpy(&buf[pos], &addr_in6->sin6_addr.s6_addr, 16);
        pos += 16;
        memcpy(&buf[pos], &addr_in6->sin6_port, 2);
        pos += 2;
    } else {
        errno = EAFNOSUPPORT;
        return -1;
    }

    // Send connect request
    if (send(sockfd, buf, pos, 0) != pos) {
        return -1;
    }

    // Read connect response
    ret = recv(sockfd, buf, sizeof(buf), 0);
    if (ret < 7) {
        return -1;
    }

    if (buf[0] != 0x05 || buf[1] != 0x00) {
        errno = ECONNREFUSED;
        return -1;
    }

    return 0;
}

// Intercepted connect()
int connect(int sockfd, const struct sockaddr *addr, socklen_t addrlen) {
    init_preload();

    if (!real_connect) {
        errno = ENOSYS;
        return -1;
    }

    // Check if this is a TCP socket
    int socktype;
    socklen_t optlen = sizeof(socktype);
    if (getsockopt(sockfd, SOL_SOCKET, SO_TYPE, &socktype, &optlen) == -1) {
        return real_connect(sockfd, addr, addrlen);
    }

    if (socktype != SOCK_STREAM) {
        // Only intercept TCP connections
        return real_connect(sockfd, addr, addrlen);
    }

    // Don't intercept connections to localhost or the proxy itself
    if (addr->sa_family == AF_INET) {
        struct sockaddr_in *addr_in = (struct sockaddr_in *)addr;
        uint32_t ip = ntohl(addr_in->sin_addr.s_addr);

        // Skip localhost (127.0.0.0/8)
        if ((ip & 0xFF000000) == 0x7F000000) {
            return real_connect(sockfd, addr, addrlen);
        }
    }

    if (getenv("TAILPROXY_VERBOSE")) {
        if (addr->sa_family == AF_INET) {
            struct sockaddr_in *addr_in = (struct sockaddr_in *)addr;
            char ip[INET_ADDRSTRLEN];
            inet_ntop(AF_INET, &addr_in->sin_addr, ip, sizeof(ip));
            fprintf(stderr, "[tailproxy] Intercepting connect to %s:%d\n",
                    ip, ntohs(addr_in->sin_port));
        }
    }

    // Save original socket flags and make socket blocking for SOCKS5 handshake
    int flags = fcntl(sockfd, F_GETFL, 0);
    int was_nonblocking = (flags != -1 && (flags & O_NONBLOCK));
    if (was_nonblocking) {
        fcntl(sockfd, F_SETFL, flags & ~O_NONBLOCK);
    }

    // Connect to SOCKS5 proxy
    struct sockaddr_in proxy_addr;
    memset(&proxy_addr, 0, sizeof(proxy_addr));
    proxy_addr.sin_family = AF_INET;
    proxy_addr.sin_port = htons(proxy_port);
    inet_pton(AF_INET, proxy_host, &proxy_addr.sin_addr);

    int ret = real_connect(sockfd, (struct sockaddr *)&proxy_addr, sizeof(proxy_addr));
    if (ret != 0 && errno != EINPROGRESS) {
        if (getenv("TAILPROXY_VERBOSE")) {
            fprintf(stderr, "[tailproxy] Failed to connect to proxy: %s\n", strerror(errno));
        }
        if (was_nonblocking) {
            fcntl(sockfd, F_SETFL, flags);
        }
        return -1;
    }

    // If connect returned EINPROGRESS (shouldn't happen now since we're blocking), wait for it
    if (ret != 0 && errno == EINPROGRESS) {
        struct pollfd pfd = { .fd = sockfd, .events = POLLOUT };
        if (poll(&pfd, 1, 30000) <= 0) {
            if (was_nonblocking) {
                fcntl(sockfd, F_SETFL, flags);
            }
            errno = ETIMEDOUT;
            return -1;
        }
        // Check if connect succeeded
        int error = 0;
        socklen_t errlen = sizeof(error);
        getsockopt(sockfd, SOL_SOCKET, SO_ERROR, &error, &errlen);
        if (error != 0) {
            if (was_nonblocking) {
                fcntl(sockfd, F_SETFL, flags);
            }
            errno = error;
            return -1;
        }
    }

    // Perform SOCKS5 handshake
    if (socks5_connect(sockfd, addr, addrlen) != 0) {
        if (getenv("TAILPROXY_VERBOSE")) {
            fprintf(stderr, "[tailproxy] SOCKS5 handshake failed: %s\n", strerror(errno));
        }
        if (was_nonblocking) {
            fcntl(sockfd, F_SETFL, flags);
        }
        return -1;
    }

    // Restore original socket flags
    if (was_nonblocking) {
        fcntl(sockfd, F_SETFL, flags);
    }

    return 0;
}

// Intercepted getaddrinfo() - return original results
int getaddrinfo(const char *node, const char *service,
                const struct addrinfo *hints, struct addrinfo **res) {
    init_preload();

    if (!real_getaddrinfo) {
        return EAI_SYSTEM;
    }

    return real_getaddrinfo(node, service, hints, res);
}

// Intercepted gethostbyname() - return original results
struct hostent *gethostbyname(const char *name) {
    init_preload();

    if (!real_gethostbyname) {
        h_errno = NO_RECOVERY;
        return NULL;
    }

    return real_gethostbyname(name);
}

// Constructor - called when library is loaded
__attribute__((constructor))
static void tailproxy_init(void) {
    init_preload();
}
