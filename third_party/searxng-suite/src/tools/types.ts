export type ToolStatus = "success" | "aborted" | "error";

export interface SearchToolDetails {
  query: string;
  status: ToolStatus;
  resultCount?: number;
  error?: string;
}

export interface ExtractToolDetails {
  url: string;
  status: ToolStatus;
  contentType?: string;
  byteLength?: number;
  error?: string;
}
