// @vitest-environment jsdom

import { render } from "@testing-library/react";
import React from "react";
import { describe, expect, it, vi } from "vitest";

import { useResourceManager } from "./useResourceManager";

interface Resource {
  id: string;
}

function TestComponent({
  tick,
  factory,
  cleanup,
}: {
  tick: number;
  factory: () => Resource;
  cleanup: (resource: Resource) => void;
}) {
  useResourceManager(factory, cleanup);
  return <div data-testid="tick">{tick}</div>;
}

describe("useResourceManager", () => {
  it("creates resource once and cleans up on unmount", () => {
    const resource: Resource = { id: "resource-1" };
    const factory = vi.fn(() => resource);
    const cleanup = vi.fn();

    const { rerender, unmount } = render(
      <TestComponent tick={1} factory={factory} cleanup={cleanup} />,
    );

    rerender(<TestComponent tick={2} factory={factory} cleanup={cleanup} />);
    rerender(<TestComponent tick={3} factory={factory} cleanup={cleanup} />);

    expect(factory).toHaveBeenCalledTimes(1);
    expect(cleanup).not.toHaveBeenCalled();

    unmount();

    expect(cleanup).toHaveBeenCalledTimes(1);
    expect(cleanup).toHaveBeenCalledWith(resource);
  });
});
