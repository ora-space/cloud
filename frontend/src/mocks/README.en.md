# mocks: Browser simulation for unrealized business APIs

[中文](README.md) | [English](README.en.md)

This module provides MSW data and handlers for task, project, workspace, and other screens that have not yet moved to real APIs. Requests are isolated under the `/mock-api` namespace and must not intercept `/auth/*` or the real `/api/v1/me` endpoint.

Authentication is outside the simulation boundary: login, logout, current user, the session cookie, and error status all come from Gateway and Cloud. New handlers must preserve that boundary and add coverage in `handlers.test.ts` or the data-store tests.
