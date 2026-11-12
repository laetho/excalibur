// Inline SVG icons for the shell, drawn in Excalidraw's stroked style so they
// match the canvas chrome. They inherit `currentColor` and are sized by CSS.
import type { SVGProps } from 'react'

type IconProps = SVGProps<SVGSVGElement>

function Icon({ children, ...props }: IconProps) {
  return (
    <svg
      viewBox="0 0 24 24"
      width="1em"
      height="1em"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.8}
      strokeLinecap="round"
      strokeLinejoin="round"
      {...props}
    >
      {children}
    </svg>
  )
}

export function FilterIcon(props: IconProps) {
  return (
    <Icon data-icon="filter" {...props}>
      <polygon points="22 3 2 3 10 12.46 10 19 14 21 14 12.46 22 3" />
    </Icon>
  )
}

export function SearchIcon(props: IconProps) {
  return (
    <Icon data-icon="search" {...props}>
      <circle cx="11" cy="11" r="7" />
      <line x1="21" y1="21" x2="16.65" y2="16.65" />
    </Icon>
  )
}

export function FolderIcon(props: IconProps) {
  return (
    <Icon data-icon="folder" {...props}>
      <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
    </Icon>
  )
}

export function DatabaseIcon(props: IconProps) {
  return (
    <Icon data-icon="database" {...props}>
      <ellipse cx="12" cy="5.5" rx="8" ry="3" />
      <path d="M4 5.5v6c0 1.66 3.58 3 8 3s8-1.34 8-3v-6" />
      <path d="M4 11.5v6c0 1.66 3.58 3 8 3s8-1.34 8-3v-6" />
    </Icon>
  )
}

export function RefreshIcon(props: IconProps) {
  return (
    <Icon data-icon="refresh" {...props}>
      <path d="M23 4v6h-6" />
      <path d="M1 20v-6h6" />
      <path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15" />
    </Icon>
  )
}

export function PlusIcon(props: IconProps) {
  return (
    <Icon data-icon="plus" {...props}>
      <line x1="12" y1="5" x2="12" y2="19" />
      <line x1="5" y1="12" x2="19" y2="12" />
    </Icon>
  )
}

export function CloseIcon(props: IconProps) {
  return (
    <Icon data-icon="close" {...props}>
      <line x1="18" y1="6" x2="6" y2="18" />
      <line x1="6" y1="6" x2="18" y2="18" />
    </Icon>
  )
}

export function AppearanceIcon(props: IconProps) {
  return (
    <Icon data-icon="appearance" {...props}>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 3a9 9 0 0 0 0 18z" fill="currentColor" stroke="none" />
    </Icon>
  )
}

export function UsersIcon(props: IconProps) {
  return (
    <Icon data-icon="users" {...props}>
      <circle cx="9" cy="8" r="3.5" />
      <path d="M2.5 20a6.5 6.5 0 0 1 13 0" />
      <path d="M16 4.6a3.5 3.5 0 0 1 0 6.8" />
      <path d="M18.5 14.2A6.5 6.5 0 0 1 21.5 20" />
    </Icon>
  )
}

export function SparklesIcon(props: IconProps) {
  return (
    <Icon data-icon="sparkles" {...props}>
      <path d="M10 3.5l1.6 4.4L16 9.5l-4.4 1.6L10 15.5l-1.6-4.4L4 9.5l4.4-1.6z" />
      <path d="M18 14l.8 2.2L21 17l-2.2.8L18 20l-.8-2.2L15 17l2.2-.8z" />
    </Icon>
  )
}

export function InfoIcon(props: IconProps) {
  return (
    <Icon data-icon="info" {...props}>
      <circle cx="12" cy="12" r="9" />
      <line x1="12" y1="11" x2="12" y2="16.5" />
      <line x1="12" y1="7.5" x2="12" y2="7.5" />
    </Icon>
  )
}

export function MinusIcon(props: IconProps) {
  return (
    <Icon data-icon="minus" {...props}>
      <line x1="5" y1="12" x2="19" y2="12" />
    </Icon>
  )
}

export function ArrowUpIcon(props: IconProps) {
  return (
    <Icon data-icon="arrow-up" strokeWidth={2.2} {...props}>
      <line x1="12" y1="19" x2="12" y2="5" />
      <polyline points="6 11 12 5 18 11" />
    </Icon>
  )
}

export function ChevronDownIcon(props: IconProps) {
  return (
    <Icon data-icon="chevron-down" {...props}>
      <polyline points="6 9 12 15 18 9" />
    </Icon>
  )
}
