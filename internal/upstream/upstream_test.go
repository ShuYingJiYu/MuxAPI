package upstream

import (
	"net/http"
	"testing"
)

func TestIsFailureStatus(t *testing.T) {
	// 应触发故障切换/熔断的状态码
	fail := []int{
		http.StatusUnauthorized,        // 401 凭证失效
		http.StatusPaymentRequired,     // 402 需付费
		http.StatusForbidden,           // 403 余额不足/无权限
		http.StatusRequestTimeout,      // 408 上游超时
		http.StatusTooManyRequests,     // 429 限流
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
	}
	for _, c := range fail {
		if !IsFailureStatus(c) {
			t.Errorf("状态码 %d 应判为上游失败", c)
		}
	}
	// 不应触发切换的状态码（成功 / 客户端请求本身的问题，透传给客户端）
	pass := []int{
		http.StatusOK,                  // 200
		http.StatusCreated,             // 201
		http.StatusBadRequest,          // 400 请求畸形
		http.StatusNotFound,            // 404 模型/路径不存在
		http.StatusUnprocessableEntity, // 422
	}
	for _, c := range pass {
		if IsFailureStatus(c) {
			t.Errorf("状态码 %d 不应判为上游失败", c)
		}
	}
}
