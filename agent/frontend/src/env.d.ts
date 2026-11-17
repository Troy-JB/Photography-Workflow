/// <reference types="vite/client" />

declare module '*.vue' {
  import type { DefineComponent } from 'vue'
  const component: DefineComponent<{}, {}, any>
  export default component
}

interface WailsBridge {
  [method: string]: (...args: unknown[]) => Promise<unknown>
}

interface WailsRuntime {
  EventsOn?: (name: string, callback: (...data: unknown[]) => void) => void
  EventsOff?: (name: string) => void
}

interface Window {
  go?: { binder?: { App?: WailsBridge } }
  runtime?: WailsRuntime
}
