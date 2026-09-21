# layout: Authenticated application shell

[中文](README.md) | [English](README.en.md)

This module composes the sidebar, page header, and route outlet. Before rendering a workspace, `DashboardLayout` verifies the real Cloud session through `/api/v1/me`: a 401 goes to the automatic login screen while preserving the current in-app path, a 403 stops relogin and reports a disabled account, and a transient failure offers only an explicit retry.

`AppSidebar` receives the Cloud `User`, displays the `displayName` snapshotted at first JIT creation, and revokes the Gateway session through the real `POST /auth/logout`. Business navigation may still use simulated data, but this module must never store tokens or duplicate authentication state.

Tests cover current-user display, unauthenticated navigation, failure handling, and local logout.
