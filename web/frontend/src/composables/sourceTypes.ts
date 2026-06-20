// Mirrors the Go JSON contract in internal/source/describe.go
// (doc multi-source-web-form-design §4). The frontend renders the connection
// form dynamically from these; the backend is the single source of truth.

export interface Option {
  label: string
  value: string
}

export interface Capabilities {
  schema: boolean
  data: boolean
  cdc: boolean
}

export interface FieldSpec {
  key: string
  label: string
  type: string // text|number|password|select|switch
  required?: boolean
  default?: unknown
  placeholder?: string
  options?: Option[]
  help?: string
  group: string // common|source (advanced reserved for a later batch)
}

export interface SourceMeta {
  name: string
  displayName: string
  implemented: boolean
  defaultPort: number
  fields: FieldSpec[]
  capabilities: Capabilities
  notImplMsg?: string
}
