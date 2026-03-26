---
name: code-review
description: Perform thorough code reviews with security, performance, and maintainability analysis. Use when user asks to review code, check for bugs, or audit a codebase.
tags: security, quality
---

# Code Review Skill

You now have expertise in conducting comprehensive code reviews. Follow this structured approach:

## Review Checklist

### 1. Security (Critical)
- Injection vulnerabilities: SQL, command, XSS
- Hardcoded credentials or secrets
- Missing access controls
- Sensitive data in logs or error messages

### 2. Correctness
- Logic errors and edge cases
- Error handling completeness
- Null/nil pointer risks
- Off-by-one errors

### 3. Performance
- Unnecessary allocations in hot paths
- N+1 query patterns
- Missing indexes on DB queries
- Unbounded loops or recursion

### 4. Maintainability
- Function/method length (prefer < 50 lines)
- Clear naming conventions
- Adequate comments for complex logic
- Test coverage for critical paths

## Output Format

Structure your review as:
1. **Summary** - 2-3 sentence overview
2. **Critical Issues** - Must fix before merge
3. **Suggestions** - Nice to have improvements
4. **Positives** - What's done well
