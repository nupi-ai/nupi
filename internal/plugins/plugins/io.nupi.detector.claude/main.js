// Claude Code detection plugin
module.exports = {
    name: "Claude Code",
    icon: "icon.svg",

    // Command that launches Claude Code
    commands: ["claude"],

    // Detect Claude Code from output
    detect: function(output) {
        // Strip ANSI escape codes for cleaner pattern matching
        // This regex removes color codes, cursor codes, etc.
        var cleaned = output.replace(/\x1b\[[0-9;]*[a-zA-Z]/g, '');

        // Method 1: Old interface detection (pre-v2.0)
        var oldInterface = cleaned.indexOf("✻ Welcome to Claude Code!") !== -1 &&
                          cleaned.indexOf("/help for help") !== -1 &&
                          cleaned.indexOf("/status for your current setup") !== -1;

        // Method 2: New interface detection (v2.0.0+)
        // Check for "Claude Code vX.Y.Z" pattern - this is the most reliable identifier
        var versionPattern = /Claude Code v\d+\.\d+\.\d+/;
        var newInterface = versionPattern.test(cleaned);

        // Return true if either old or new interface is detected
        return oldInterface || newInterface;
    }
};
