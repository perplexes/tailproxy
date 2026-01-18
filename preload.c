#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <netdb.h>
#include <unistd.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <pthread.h>

// Function pointers for original syscalls
static int (*real_connect)(int, const struct sockaddr *, socklen_t) = NULL;
static int (*real_bind)(int, const struct sockaddr *, socklen_t) = NULL;
static int (*real_listen)(int, int) = NULL;
static int (*real_close)(int) = NULL;
static int (*real_getaddrinfo)(const char *, const char *, const struct addrinfo *, struct addrinfo **) = NULL;
static struct hostent *(*real_gethostbyname)(const char *) = NULL;

// FD tracking structure
#define MAX_FDS 65536
typedef struct {
    int is_tcp;
    int is_listener;
    int family;
    int port;
} fd_info_t;

static fd_info_t fd_table[MAX_FDS];
static pthread_mutex_t fd_table_lock = PTHREAD_MUTEX_INITIALIZER;

// Configuration
static char *proxy_host = "127.0.0.1";
static int proxy_port = 1080;
static int initialized = 0;
static int export_enabled = 0;
static char *control_socket = NULL;
static int control_fd = -1;

// Helper to send control message
static void send_control_message(const char *msg) {
    if (!export_enabled || !control_socket) {
        return;
    }

    // Lazy init control socket
    if (control_fd < 0) {
        control_fd = socket(AF_UNIX, SOCK_STREAM, 0);
        if (control_fd < 0) {
            return;
        }

        struct sockaddr_un addr;
        memset(&addr, 0, sizeof(addr));
        addr.sun_family = AF_UNIX;
        strncpy(addr.sun_path, control_socket, sizeof(addr.sun_path) - 1);

        if (connect(control_fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
            close(control_fd);
            control_fd = -1;
            return;
        }
    }

    // Send message (best effort, don't block the app)
    int len = strlen(msg);
    send(control_fd, msg, len, MSG_DONTWAIT | MSG_NOSIGNAL);
}

// Initialize the library
static void init_preload(void) {
    if (initialized) return;

    // Load original function pointers
    real_connect = dlsym(RTLD_NEXT, "connect");
    real_bind = dlsym(RTLD_NEXT, "bind");
    real_listen = dlsym(RTLD_NEXT, "listen");
    real_close = dlsym(RTLD_NEXT, "close");
    real_getaddrinfo = dlsym(RTLD_NEXT, "getaddrinfo");
    real_gethostbyname = dlsym(RTLD_NEXT, "gethostbyname");

    // Initialize FD table
    memset(fd_table, 0, sizeof(fd_table));

    // Read proxy configuration from environment
    char *env_port = getenv("TAILPROXY_PORT");
    if (env_port) {
        proxy_port = atoi(env_port);
    }

    char *env_host = getenv("TAILPROXY_HOST");
    if (env_host) {
        proxy_host = env_host;
    }

    // Check if export mode is enabled
    if (getenv("TAILPROXY_EXPORT_LISTENERS")) {
        export_enabled = 1;
        control_socket = getenv("TAILPROXY_CONTROL_SOCK");
    }

    initialized = 1;

    if (getenv("TAILPROXY_VERBOSE")) {
        fprintf(stderr, "[tailproxy] Initialized: proxy=%s:%d, export=%d\n",
                proxy_host, proxy_port, export_enabled);
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

// Intercepted bind()
int bind(int sockfd, const struct sockaddr *addr, socklen_t addrlen) {
    init_preload();

    if (!real_bind) {
        errno = ENOSYS;
        return -1;
    }

    // If export mode not enabled, just pass through
    if (!export_enabled) {
        return real_bind(sockfd, addr, addrlen);
    }

    // Check if this is a TCP socket
    int socktype;
    socklen_t optlen = sizeof(socktype);
    if (getsockopt(sockfd, SOL_SOCKET, SO_TYPE, &socktype, &optlen) == -1) {
        return real_bind(sockfd, addr, addrlen);
    }

    if (socktype != SOCK_STREAM) {
        // Only intercept TCP sockets
        return real_bind(sockfd, addr, addrlen);
    }

    // Track as TCP socket
    pthread_mutex_lock(&fd_table_lock);
    if (sockfd >= 0 && sockfd < MAX_FDS) {
        fd_table[sockfd].is_tcp = 1;
        fd_table[sockfd].family = addr->sa_family;
    }
    pthread_mutex_unlock(&fd_table_lock);

    // Rewrite bind address to loopback
    if (addr->sa_family == AF_INET) {
        struct sockaddr_in *addr_in = (struct sockaddr_in *)addr;
        uint32_t ip = ntohl(addr_in->sin_addr.s_addr);

        // If not already loopback, rewrite to 127.0.0.1
        if ((ip & 0xFF000000) != 0x7F000000) {
            struct sockaddr_in new_addr;
            memcpy(&new_addr, addr_in, sizeof(new_addr));
            new_addr.sin_addr.s_addr = htonl(0x7F000001); // 127.0.0.1

            if (getenv("TAILPROXY_VERBOSE")) {
                char orig_ip[INET_ADDRSTRLEN];
                inet_ntop(AF_INET, &addr_in->sin_addr, orig_ip, sizeof(orig_ip));
                fprintf(stderr, "[tailproxy] Rewriting bind from %s to 127.0.0.1:%d\n",
                        orig_ip, ntohs(addr_in->sin_port));
            }

            return real_bind(sockfd, (struct sockaddr *)&new_addr, addrlen);
        }
    } else if (addr->sa_family == AF_INET6) {
        struct sockaddr_in6 *addr_in6 = (struct sockaddr_in6 *)addr;

        // Check if not already ::1
        struct in6_addr loopback = IN6ADDR_LOOPBACK_INIT;
        if (memcmp(&addr_in6->sin6_addr, &loopback, sizeof(struct in6_addr)) != 0) {
            struct sockaddr_in6 new_addr;
            memcpy(&new_addr, addr_in6, sizeof(new_addr));
            new_addr.sin6_addr = loopback;

            if (getenv("TAILPROXY_VERBOSE")) {
                fprintf(stderr, "[tailproxy] Rewriting IPv6 bind to ::1:%d\n",
                        ntohs(addr_in6->sin6_port));
            }

            return real_bind(sockfd, (struct sockaddr *)&new_addr, addrlen);
        }
    }

    // Already loopback, pass through
    return real_bind(sockfd, addr, addrlen);
}

// Intercepted listen()
int listen(int sockfd, int backlog) {
    init_preload();

    if (!real_listen) {
        errno = ENOSYS;
        return -1;
    }

    // Call real listen first
    int ret = real_listen(sockfd, backlog);
    if (ret != 0) {
        return ret;
    }

    // If export mode enabled and this is a TCP socket, notify Go
    if (export_enabled && sockfd >= 0 && sockfd < MAX_FDS) {
        pthread_mutex_lock(&fd_table_lock);
        if (fd_table[sockfd].is_tcp) {
            fd_table[sockfd].is_listener = 1;

            // Get actual bound port
            struct sockaddr_storage ss;
            socklen_t slen = sizeof(ss);
            if (getsockname(sockfd, (struct sockaddr *)&ss, &slen) == 0) {
                int port = 0;
                const char *family_str = "tcp4";

                if (ss.ss_family == AF_INET) {
                    struct sockaddr_in *sin = (struct sockaddr_in *)&ss;
                    port = ntohs(sin->sin_port);
                    family_str = "tcp4";
                } else if (ss.ss_family == AF_INET6) {
                    struct sockaddr_in6 *sin6 = (struct sockaddr_in6 *)&ss;
                    port = ntohs(sin6->sin6_port);
                    family_str = "tcp6";
                }

                fd_table[sockfd].port = port;

                if (port > 0) {
                    char msg[128];
                    snprintf(msg, sizeof(msg), "LISTEN %s %d\n", family_str, port);
                    pthread_mutex_unlock(&fd_table_lock);

                    send_control_message(msg);

                    if (getenv("TAILPROXY_VERBOSE")) {
                        fprintf(stderr, "[tailproxy] Notifying listener on port %d\n", port);
                    }

                    pthread_mutex_lock(&fd_table_lock);
                }
            }
        }
        pthread_mutex_unlock(&fd_table_lock);
    }

    return ret;
}

// Intercepted close()
int close(int fd) {
    init_preload();

    if (!real_close) {
        errno = ENOSYS;
        return -1;
    }

    // If export mode enabled and this was a listener, notify Go
    if (export_enabled && fd >= 0 && fd < MAX_FDS) {
        pthread_mutex_lock(&fd_table_lock);
        if (fd_table[fd].is_listener && fd_table[fd].port > 0) {
            const char *family_str = (fd_table[fd].family == AF_INET) ? "tcp4" : "tcp6";
            int port = fd_table[fd].port;

            pthread_mutex_unlock(&fd_table_lock);

            char msg[128];
            snprintf(msg, sizeof(msg), "CLOSE %s %d\n", family_str, port);
            send_control_message(msg);

            if (getenv("TAILPROXY_VERBOSE")) {
                fprintf(stderr, "[tailproxy] Notifying close of listener on port %d\n", port);
            }

            pthread_mutex_lock(&fd_table_lock);
        }

        // Clear FD entry
        memset(&fd_table[fd], 0, sizeof(fd_info_t));
        pthread_mutex_unlock(&fd_table_lock);
    }

    return real_close(fd);
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
