declare module 'asciinema-player' {
  export interface PlayerOptions {
    autoPlay?: boolean;
    loop?: boolean;
    speed?: number;
    idleTimeLimit?: number;
    theme?: 'asciinema' | 'monokai' | 'solarized-dark' | 'solarized-light' | string;
    fit?: 'width' | 'height' | 'both' | false;
    terminalFontSize?: string;
    terminalLineHeight?: number;
    controls?: boolean | 'auto';
  }

  export interface Player {
    dispose?: () => void;
  }

  export function create(
    src: string,
    container: HTMLElement,
    options?: PlayerOptions
  ): Player;

  export function create(
    src: string,
    container: HTMLElement,
    workerUrl: string,
    options?: PlayerOptions
  ): Player;
}
