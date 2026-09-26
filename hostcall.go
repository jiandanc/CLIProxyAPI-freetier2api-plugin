package main

// 本文件实现插件侧对宿主回调（host.*）的调用。
//
// 唯一实现是 nativeHostCallScoped，它做三件事：
//  1. 申请并发准入槽位（宿主回调不可取消，必须限流，见 hostgate.go）；
//  2. 在载荷里注入 host_callback_id（把回调绑定到具体请求作用域）；
//  3. 调用 C ABI 并把响应解成 JSON。

import (
	"encoding/json"
	"fmt"
	"unsafe"

	"workbuddy2api-plugin/internal/logger"
)

/*
#include "bridge.h"
*/
import "C"

// hostCallScopedImpl 指向真实的 CGO 宿主调用。
//
// 刻意做成可替换的包级变量：单元测试把它换成内存假宿主，
// 从而在没有 CPA 进程参与的情况下验证出站 HTTP、任务上报、账号枚举等全部链路。
var hostCallScopedImpl = nativeHostCallScoped

// callHost 发起一次不带请求作用域的宿主回调（后台循环、管理接口等场景）。
func callHost(method string, payload any) (json.RawMessage, error) {
	return hostCallScopedImpl("", method, payload)
}

// callHostScoped 发起一次带请求作用域的宿主回调。
//
// host_callback_id 把宿主调用绑定到具体请求：流式转发（host.stream.emit）与
// 出站 HTTP（host.http.do_stream）都必须携带它，否则宿主无法把上游请求/响应
// 关联回原始请求，也无法在请求结束时回收流资源。
func callHostScoped(callbackID, method string, payload any) (json.RawMessage, error) {
	return hostCallScopedImpl(callbackID, method, payload)
}

// nativeHostCallScoped 是宿主回调的真实实现。
func nativeHostCallScoped(callbackID string, method string, payload any) (json.RawMessage, error) {
	// 整个 CGO 调用期间持有准入槽位：宿主回调不可取消，
	// 超时返回的调用仍会占用一个 OS 线程直到宿主返回。
	if errAcquire := tryAcquireHostCall(); errAcquire != nil {
		return nil, errAcquire
	}
	defer releaseHostCall()

	rawPayload, errMarshal := marshalHostPayload(callbackID, payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("%s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("%s: allocate host callback payload", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}

	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("%s: host callback returned no response, code=%d", method, int(callCode))
	}
	result, errDecode := decodeEnvelopeResult(rawResponse)
	if errDecode != nil {
		return nil, fmt.Errorf("%s: %w", method, errDecode)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("%s: host callback returned code=%d", method, int(callCode))
	}
	return result, nil
}

// marshalHostPayload 把载荷编码为 JSON，并按需注入 host_callback_id。
//
// 载荷是 any（各方法自有的请求结构体），所以采用「先序列化再塞字段」的方式注入，
// 而不是给每个结构体都加上这个字段。
func marshalHostPayload(callbackID string, payload any) ([]byte, error) {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback payload: %w", errMarshal)
	}
	if callbackID == "" {
		return raw, nil
	}
	fields := map[string]json.RawMessage{}
	if len(raw) > 0 && string(raw) != "null" {
		if errUnmarshal := json.Unmarshal(raw, &fields); errUnmarshal != nil {
			return nil, fmt.Errorf("decode host callback payload: %w", errUnmarshal)
		}
	}
	encodedID, errID := json.Marshal(callbackID)
	if errID != nil {
		return nil, fmt.Errorf("marshal host callback id: %w", errID)
	}
	fields["host_callback_id"] = encodedID
	return json.Marshal(fields)
}

// decodeHostResult 把宿主回调的 result 解成目标结构。
func decodeHostResult(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return nil
	}
	if errUnmarshal := json.Unmarshal(raw, target); errUnmarshal != nil {
		return fmt.Errorf("decode host result: %w", errUnmarshal)
	}
	return nil
}

// logHostCallFailure 记录一次宿主回调失败（供调用方在降级路径上使用）。
func logHostCallFailure(method string, err error) {
	if err == nil {
		return
	}
	logger.Debug("host callback %s failed: %v", method, err)
}
