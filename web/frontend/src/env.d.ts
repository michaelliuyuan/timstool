/// <reference types="vite/client" />

// Build-time app version, stamped by vite define from package.json "version"
// (MS-11i pen1: single source of truth — bump package.json, never App.vue).
declare const __APP_VERSION__: string
