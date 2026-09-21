# mocks：未真实化业务的浏览器模拟

[中文](README.md) | [English](README.en.md)

本模块为任务、项目、工作区等尚未接入真实 API 的页面提供 MSW 数据与 handler。请求经 `/mock-api` 命名空间隔离，不能拦截 `/auth/*` 或真实 `/api/v1/me`。

认证不属于模拟范围：登录、退出、当前用户、会话 Cookie 与错误状态全部来自 Gateway 和 Cloud。新增 handler 时必须保持这一边界，并在 `handlers.test.ts` 或数据存储测试中覆盖行为。
