// Shim so TypeScript understands .vue imports. Without this vue-tsc fails with
// "Cannot find module '@/views/X.vue'". Vite itself does not need the shim at
// runtime; this is purely for type-checking.
declare module '*.vue' {
  import type { DefineComponent } from 'vue'
  const component: DefineComponent<Record<string, never>, Record<string, never>, unknown>
  export default component
}