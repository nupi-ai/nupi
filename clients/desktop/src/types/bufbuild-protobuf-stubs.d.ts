declare module "@bufbuild/protobuf" {
  export type Message<TName extends string = string> = {
    readonly $typeName?: TName;
  } & Record<string, unknown>;
}

declare module "@bufbuild/protobuf/codegenv2" {
  export type GenEnum<T = unknown> = unknown;
  export type GenFile = unknown;
  export type GenMessage<T = unknown> = unknown;
  export type GenService<T = unknown> = unknown;
}

declare module "@bufbuild/protobuf/wkt" {
  export type EmptySchema = unknown;
  export type Timestamp = unknown;
}
