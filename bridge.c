/* bridge.c —— 宿主函数表的存储与访问包装。
 *
 * stored_host 是本文件私有的 static 变量：Go 侧不接触其内部布局，
 * 只通过 call_host_api / free_host_buffer 两个函数访问。
 * 这样即使宿主 ABI 的结构体布局变化，也只需改这一个文件。
 */
#include "bridge.h"

static const cliproxy_host_api* stored_host;

void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

void clear_host_api(void) {
	stored_host = NULL;
}

int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
