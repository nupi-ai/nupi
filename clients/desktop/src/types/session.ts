export interface Session {
  id: string;
  command: string;
  args: string[];
  work_dir?: string;
  tool?: string;
  tool_icon?: string;
  tool_icon_data?: string; // Base64 encoded icon data
  status: string;
  start_time?: string;
  pid?: number;
  exit_code?: number;
  mode?: string;
}
