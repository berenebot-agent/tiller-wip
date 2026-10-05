"""Normalized public records; None represents unavailable capability or usage data."""

from dataclasses import dataclass, field
from typing import Any, Literal

JSON = dict[str, Any]
Capability = bool | None


@dataclass(frozen=True)
class Limits:
    """Positive byte limits for requests, responses, SSE, and tool accumulation."""
    body_bytes: int = 8 * 1024 * 1024
    line_bytes: int = 1024 * 1024
    event_bytes: int = 2 * 1024 * 1024
    argument_bytes: int = 1024 * 1024
    catalog_pages: int = 100

    def __post_init__(self) -> None:
        if any(type(value) is not int or value <= 0 for value in vars(self).values()):
            raise ValueError("Limits must be positive integers")


@dataclass(frozen=True)
class Reasoning:
    """Efforts are unrestricted only when efforts_known is true and efforts is None."""
    efforts: tuple[str, ...] | None = None
    efforts_known: bool = False
    budget_min: int | None = None
    budget_max: int | None = None
    supports_max_tokens: Capability = None
    toggle: Capability = None
    modes: tuple[str, ...] | None = None
    default_effort: str | None = None
    default_enabled: Capability = None
    mandatory: Capability = None
    raw: JSON = field(default_factory=dict, repr=False)


@dataclass(frozen=True)
class ModelFilter:
    """Requirements satisfied only by known-positive capabilities and known limits."""
    vision: bool = False
    tools: bool = False
    reasoning: bool = False
    structured_output: bool = False
    json_object: bool = False
    min_context: int = 0
    min_output: int = 0
    input_modalities: tuple[str, ...] = ()
    output_modalities: tuple[str, ...] = ()


@dataclass(frozen=True)
class Model:
    """Provider metadata without ID-based inference; raw preserves native fields."""
    id: str
    name: str | None = None
    context_length: int | None = None
    max_output_tokens: int | None = None
    input_modalities: tuple[str, ...] | None = None
    output_modalities: tuple[str, ...] | None = None
    vision: Capability = None
    tools: Capability = None
    reasoning: Capability = None
    structured_output: Capability = None
    json_object: Capability = None
    reasoning_metadata: Reasoning = field(default_factory=Reasoning)
    raw: JSON = field(default_factory=dict, repr=False)

    def matches(self, requirements: ModelFilter) -> bool:
        """Return whether all requested capabilities, modalities, and limits are met."""
        for key in ("vision", "tools", "reasoning", "structured_output", "json_object"):
            if getattr(requirements, key) and getattr(self, key) is not True:
                return False
        if requirements.min_context > 0 and (
            self.context_length is None or self.context_length < requirements.min_context
        ):
            return False
        if requirements.min_output > 0 and (
            self.max_output_tokens is None or self.max_output_tokens < requirements.min_output
        ):
            return False
        for key in ("input_modalities", "output_modalities"):
            wanted = getattr(requirements, key)
            actual = getattr(self, key)
            if wanted and (actual is None or not set(wanted).issubset(actual)):
                return False
        return True


@dataclass(frozen=True)
class Catalog:
    """Last successful catalog with a Unix wall-clock fetch timestamp."""
    models: tuple[Model, ...]
    fetched_at: float

    def filter(self, requirements: ModelFilter) -> tuple[Model, ...]:
        """Select models without treating unknown support as permission."""
        return tuple(model for model in self.models if model.matches(requirements))


@dataclass(frozen=True)
class Usage:
    """Optional token/cache accounting with complete native accounting retained."""
    prompt_tokens: int | None = None
    completion_tokens: int | None = None
    total_tokens: int | None = None
    raw: JSON = field(default_factory=dict, repr=False)
    cached_tokens: int | None = None
    cache_read_input_tokens: int | None = None
    cache_creation_input_tokens: int | None = None


@dataclass(frozen=True, repr=False)
class ToolCall:
    """Validated function call; arguments remain native JSON text, never executed."""
    id: str | None
    name: str | None
    arguments: str = field(repr=False)
    raw: JSON = field(repr=False)

    def __repr__(self) -> str:
        return "ToolCall()"


@dataclass(frozen=True, repr=False)
class ChatResult:
    """First assistant choice plus native message for lossless conversation replay."""
    content: Any = field(repr=False)
    tool_calls: tuple[ToolCall, ...] = field(repr=False)
    assistant_message: JSON = field(repr=False)
    reasoning: Any = field(repr=False)
    model: str | None
    id: str | None
    finish_reason: str | None
    usage: Usage | None
    request_id: str | None
    raw: JSON = field(repr=False)

    def __repr__(self) -> str:
        return "ChatResult()"


@dataclass(frozen=True, repr=False)
class StreamEvent:
    """Ordered typed fragment with raw SSE data and opaque reasoning preserved."""
    kind: Literal["text", "reasoning", "tool", "finish", "usage", "frame", "done"]
    text: str | None = field(default=None, repr=False)
    choice_index: int | None = None
    tool_index: int | None = None
    tool_id: str | None = None
    tool_name: str | None = None
    arguments: str | None = field(default=None, repr=False)
    reasoning_details: Any = field(default=None, repr=False)
    finish_reason: str | None = None
    usage: Usage | None = None
    raw: JSON | None = field(default=None, repr=False)
    raw_frame: str | None = field(default=None, repr=False)
    request_id: str | None = field(default=None, repr=False)

    def __repr__(self) -> str:
        return f"StreamEvent(kind={self.kind!r})"
