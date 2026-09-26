/* bridge.h —— CPA 插件宿主的 C ABI 声明。
 *
 * 这里只放类型与函数声明。C 函数的**定义**放在 bridge.c：
 * cgo 在遇到 //export 时会把 preamble 复制到多个生成文件里，
 * 若 preamble 里带函数定义会导致重复符号（multiple definition）链接失败。
 */
#ifndef WORKBUDDY2API_BRIDGE_H
#define WORKBUDDY2API_BRIDGE_H

#include <stdint.h>
#include <stddef.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

/* 由 Go 侧实现、以 C 符号导出的三个入口。 */
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

/* 宿主函数表的访问包装（定义在 bridge.c）。 */
void store_host_api(const cliproxy_host_api* host);
void clear_host_api(void);
int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response);
void free_host_buffer(void* ptr, size_t len);

#endif /* WORKBUDDY2API_BRIDGE_H */
