# Contributing to simplyblock-operator

🎉 Thanks for your interest in contributing!

We welcome community contributions to improve the **simplyblock Kubernetes stack** — the operator, CSI
driver, shared `atlas` library, and Helm charts all live in this monorepo — and make it even better for
Kubernetes users worldwide.

This document outlines guidelines and steps to help you get started.

## 📌 Ways to Contribute

There are many ways to help:

- **Report bugs** — open an [issue](https://github.com/simplyblock/simplyblock-operator/issues) if you encounter problems.
- **Suggest features** — request improvements or new capabilities.
- **Improve documentation** — clarify instructions, fix typos, or add examples.
- **Submit code** — fix bugs, add features, or refactor code.
- **Review pull requests** — give constructive feedback on other contributions.

## 🐞 Reporting Issues

When reporting a bug, please include:

1. **Description** of the problem
2. **Which component** it affects (operator, csi-driver, atlas-lib, or helm-charts)
3. **Steps to reproduce** (if applicable)
4. **Expected behavior** vs. what actually happened
5. **Environment details**: Kubernetes distribution and version, simplyblock version, OS, etc.
6. Relevant **logs or error messages**

This helps us resolve issues faster.

## 🚀 Submitting Pull Requests

1. **Fork the repository**
   Click “Fork” on the top-right of this repo and clone your fork locally.

   ```bash
   git clone https://github.com/<your-username>/simplyblock-operator.git
   cd simplyblock-operator
   ```

2. **Create a new branch**

   ```bash
   git checkout -b feature/my-feature
   ```

3. **Make your changes**
   Ensure your code follows project guidelines (see below).

4. **Commit your changes**

   ```bash
   git commit -m "Add: short description of the change"
   ```

   Use clear, concise commit messages (conventional commits are preferred but not required).

5. **Push to your fork**

   ```bash
   git push origin feature/my-feature
   ```

6. **Open a Pull Request**
   Go to the main repo and click **New Pull Request**.
   Provide context for your changes and link any related issues.

## 🧑‍💻 Coding Guidelines

To keep the project consistent:

* Follow standard **Go** conventions; each component has its own `Makefile` with `fmt`, `vet`, and `lint` targets.
* Build and test from the repository root with `make build` and `make test`, or target a single component (e.g. `make operator-test`). Run `make help` for the full list.
* Keep code **readable and well-documented**.
* Write **small, focused commits**.
* Add or update **tests** where applicable.

## ✅ Pull Request Checklist

Before submitting, please ensure:

* [ ] Code builds and runs locally (`make build`)
* [ ] Changes are tested (`make test`; add unit or integration tests where possible)
* [ ] Code is formatted and passes lint/vet (`make fmt lint vet`)
* [ ] Documentation updated if required
* [ ] Commit message(s) are descriptive
* [ ] PR references related issues (e.g. "Fixes #123")

## 🙌 Community Guidelines

* Be respectful and constructive.
* Provide context when suggesting changes.
* Help others by answering questions and reviewing PRs.

## 📬 Questions?

If you’re unsure about anything, feel free to:

* Create an [Issue](https://github.com/simplyblock/simplyblock-operator/issues)
* Reach out via the simplyblock community

💡 Together, we can make **simplyblock** the best NVMe-first storage platform for Kubernetes!