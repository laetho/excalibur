import { useLayoutEffect, useState, type RefObject } from 'react'

/** Space kept between our canvas overlays and Excalidraw's shape toolbar. */
const TOOLBAR_GAP = 8
/** Space between Excalidraw's small-screen tool tab and anything we stack below it. */
const TAB_GAP = 8
/** Where Excalidraw puts its small-screen tool tab (5rem minus the 1rem editor padding). */
const DEFAULT_TAB_TOP = 64

export type ToolbarRoom = {
  /** The element, at its full size and position, clears the shape toolbar, and the layout isn't the small-screen one. */
  fits: boolean
  /** Top offset (in the container) for a tab stacked under Excalidraw's own right-edge tool tab. */
  tabTop: number
}

/**
 * Whether an overlay we place on top of Excalidraw (the sync badge, the Merlin button) has room beside its
 * centered shape toolbar. It measures the real toolbar, whose width depends on Excalidraw's tools and
 * fonts, and re-checks when the canvas resizes or Excalidraw's UI changes (e.g. the Library opening, which
 * moves the Merlin button). The element must stay laid out (visibility: hidden, not display: none) while it
 * doesn't fit, so it can be measured.
 */
export function useToolbarRoom(ref: RefObject<HTMLElement | null>): ToolbarRoom {
  const [room, setRoom] = useState<ToolbarRoom>({ fits: true, tabTop: DEFAULT_TAB_TOP })

  useLayoutEffect(() => {
    const element = ref.current
    const container = element?.parentElement
    if (!element || !container) return
    let frame = 0
    const check = () => {
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(() => {
        const top = container.getBoundingClientRect().top
        const tab = container.querySelector('.mobile-misc-tools-container')
        const tabTop = tab ? tab.getBoundingClientRect().bottom - top + TAB_GAP : DEFAULT_TAB_TOP
        // Small-screen layout: Excalidraw's own controls move to the edges; keep ours out of the way too.
        if (container.querySelector('.App-toolbar--mobile')) {
          setRoom((current) => (!current.fits && current.tabTop === tabTop ? current : { fits: false, tabTop }))
          return
        }
        const toolbar = container.querySelector('.App-toolbar')?.getBoundingClientRect()
        const rect = element.getBoundingClientRect()
        const fits = !toolbar || rect.right + TOOLBAR_GAP <= toolbar.left || rect.left - TOOLBAR_GAP >= toolbar.right
        setRoom((current) => (current.fits === fits && current.tabTop === tabTop ? current : { fits, tabTop }))
      })
    }
    check()
    const resize = new ResizeObserver(check)
    resize.observe(container)
    // Excalidraw switching layouts or opening its sidebar changes the DOM, not the container's size.
    const mutations = new MutationObserver(check)
    mutations.observe(container, { childList: true, subtree: true })
    element.addEventListener('transitionend', check)
    return () => {
      cancelAnimationFrame(frame)
      resize.disconnect()
      mutations.disconnect()
      element.removeEventListener('transitionend', check)
    }
  }, [ref])

  return room
}
