// useResponsive is a Vue 3 composable that detects the user's device class
// and screen size, providing reactive layout parameters for mobile-first UIs.
//
// Usage:
//   import { useResponsive } from '@/composables/useResponsive'
//   const { isMobile, isTablet, isDesktop, breakpoint } = useResponsive()
//
// The composable listens to window resize events and cleans up on scope
// disposal (onScopeDispose). It is SSR-safe: when window is undefined the
// composable returns desktop defaults.
import { onScopeDispose, ref, type Ref } from 'vue'

// Breakpoint constants (in pixels). They match the Element Plus defaults so
// that responsive utilities and the EP grid system stay aligned.
export const BreakpointXS = 480
export const BreakpointSM = 768
export const BreakpointMD = 992
export const BreakpointLG = 1200
export const BreakpointXL = 1920

// BreakpointName is the human-readable label for the current breakpoint.
export type BreakpointName = 'xs' | 'sm' | 'md' | 'lg' | 'xl'

// ResponsiveState is the reactive state returned by useResponsive.
export interface ResponsiveState {
  /** Window width in pixels. 0 when window is unavailable. */
  width: Ref<number>
  /** Window height in pixels. 0 when window is unavailable. */
  height: Ref<number>
  /** True when width < BreakpointSM (phone-sized). */
  isMobile: Ref<boolean>
  /** True when BreakpointSM <= width < BreakpointLG (tablet-sized). */
  isTablet: Ref<boolean>
  /** True when width >= BreakpointLG (desktop-sized). */
  isDesktop: Ref<boolean>
  /** True when the user agent indicates a touch-primary device. */
  isTouch: Ref<boolean>
  /** Current breakpoint name. */
  breakpoint: Ref<BreakpointName>
  /** Number of columns to use in a responsive grid at the current width. */
  gridCols: Ref<number>
}

// detectTouch inspects the user agent and navigator.maxTouchPoints to decide
// whether the device is touch-primary. It is conservative: only returns true
// when both a touch pointer is present and the UA looks mobile.
function detectTouch(): boolean {
  if (typeof window === 'undefined' || typeof navigator === 'undefined') {
    return false
  }
  const hasTouchPoints = (navigator.maxTouchPoints ?? 0) > 0
  const ua = navigator.userAgent.toLowerCase()
  const mobileUA = /android|iphone|ipad|ipod|windows phone|mobile/.test(ua)
  return hasTouchPoints && mobileUA
}

// resolveBreakpoint maps a pixel width to a BreakpointName.
function resolveBreakpoint(width: number): BreakpointName {
  if (width < BreakpointXS) return 'xs'
  if (width < BreakpointSM) return 'sm'
  if (width < BreakpointMD) return 'md'
  if (width < BreakpointLG) return 'lg'
  return 'xl'
}

// resolveGridCols returns the number of grid columns to use at a given width.
// Mobile uses a single column; tablet uses 2; desktop uses 3; wide desktop 4.
function resolveGridCols(width: number): number {
  if (width < BreakpointSM) return 1
  if (width < BreakpointMD) return 2
  if (width < BreakpointLG) return 3
  return 4
}

// initialWidth returns the current window width or 0 when window is undefined.
function initialWidth(): number {
  if (typeof window === 'undefined') return 0
  return window.innerWidth
}

// initialHeight returns the current window height or 0 when window is undefined.
function initialHeight(): number {
  if (typeof window === 'undefined') return 0
  return window.innerHeight
}

// useResponsive returns reactive refs tracking the viewport. The composable
// registers a resize listener that is automatically removed when the calling
// scope is disposed.
export function useResponsive(): ResponsiveState {
  const width = ref(initialWidth())
  const height = ref(initialHeight())
  const isTouch = ref(detectTouch())

  const isMobile = ref(width.value < BreakpointSM)
  const isTablet = ref(width.value >= BreakpointSM && width.value < BreakpointLG)
  const isDesktop = ref(width.value >= BreakpointLG)
  const breakpoint = ref<BreakpointName>(resolveBreakpoint(width.value))
  const gridCols = ref(resolveGridCols(width.value))

  // update recomputes the derived state from the current width.
  function update(): void {
    const w = typeof window === 'undefined' ? 0 : window.innerWidth
    const h = typeof window === 'undefined' ? 0 : window.innerHeight
    width.value = w
    height.value = h
    isMobile.value = w < BreakpointSM
    isTablet.value = w >= BreakpointSM && w < BreakpointLG
    isDesktop.value = w >= BreakpointLG
    breakpoint.value = resolveBreakpoint(w)
    gridCols.value = resolveGridCols(w)
  }

  // Only register the listener when window exists; otherwise we are in SSR
  // or a test environment and the refs keep their initial values.
  if (typeof window !== 'undefined') {
    const handler = (): void => update()
    window.addEventListener('resize', handler, { passive: true })
    onScopeDispose(() => {
      window.removeEventListener('resize', handler)
    })
  }

  return { width, height, isMobile, isTablet, isDesktop, isTouch, breakpoint, gridCols }
}