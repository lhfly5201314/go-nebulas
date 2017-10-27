// Copyright (C) 2017 go-nebulas authors
//
// This file is part of the go-nebulas library.
//
// the go-nebulas library is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// the go-nebulas library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with the go-nebulas library.  If not, see <http://www.gnu.org/licenses/>.
//
const Storage = require('Storage.js');
const LCS = Storage.LocalContractStorage;
const GCS = Storage.GlobalContractStorage;

var NebulasToken = function () {
    this._balances = new LCS();
};

NebulasToken.prototype = {
    init: function (totalSupply) {
        this._totalSupply = totalSupply;
        this._totalIssued = 0;
    },
    totalSupply: function () {
        return this._totalSupply;
    },
    balanceOf: function (owner) {
        return this._balances.get(owner) || 0;
    },
    transfer: function (to, value) {
        var balance = this.balanceOf(msg.sender);
        if (balance < value) {
            return false;
        }

        var finalBalance = balance - value;
        this._balances.set(msg.sender, finalBalance);
        this._balances.set(to, this.balanceOf(to) + value);
        return true;
    },
    pay: function (nass) {
        var r = nass;
        if (this._totalIssued + r > this._totalSupply) {
            return false;
        }

        this._balances.set(msg.sender, this.balanceOf(msg.sender) + r);
        this._totalIssued += r;
    }
};

var exports = module.exports = new NebulasToken();
