"""pi-supervisor Hermes plugin (ADR-0022).

Directory plugins are imported through this package init
(hermes_cli/plugins_loader.py: ``No __init__.py in <dir>``), so it is a thin
re-export of the implementation in ``plugin.py`` — the module the tools,
slash command, and tests target. The dir is named ``hermes-plugin`` (invalid
as a Python package name), so when this file is imported WITHOUT a package
context (pytest collection, direct import) the relative form cannot resolve
and the sibling is imported by name instead — same code either way.
"""

try:
    from .plugin import register  # noqa: F401
except ImportError:  # no package context (pytest collection)
    from plugin import register  # noqa: F401